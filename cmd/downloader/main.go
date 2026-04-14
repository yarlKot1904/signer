package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/redis/go-redis/v9"
	"github.com/yarlKot1904/signer/internal/config"
	"github.com/yarlKot1904/signer/internal/infra"
	"github.com/yarlKot1904/signer/internal/logutil"
	appmetrics "github.com/yarlKot1904/signer/internal/metrics"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type FileMeta struct {
	OriginalName string `json:"original_name"`
	S3Key        string `json:"s3_key"`
	MimeType     string `json:"mime_type"`
}

type SigningSession struct {
	Token       string `gorm:"primaryKey"`
	SignedS3Key string
}

var db *gorm.DB

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("Config error:", err)
	}

	appCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	appmetrics.StartServer(appCtx, cfg.MetricsPort, "Downloader", cfg.ShutdownTimeout)

	if cfg.DBDSN != "" {
		db, err = gorm.Open(postgres.Open(cfg.DBDSN), &gorm.Config{})
		if err != nil {
			log.Fatal("DB connect failed:", err)
		}
	}

	s3Client, err := infra.NewS3Client(appCtx, cfg.MinioEndpoint, cfg.MinioID, cfg.MinioSecret, cfg.MinioRegion)
	if err != nil {
		log.Fatal("S3 connect failed:", err)
	}

	redisClient, err := infra.NewRedisClient(cfg.RedisAddr)
	if err != nil {
		log.Fatal("Redis connect failed:", err)
	}
	defer func() {
		if err := redisClient.Close(); err != nil {
			log.Printf("Redis close failed: %v", err)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/download/", appmetrics.InstrumentHandlerFunc("downloader", "/download/{token}", func(w http.ResponseWriter, r *http.Request) {
		serveFile(w, r, redisClient, s3Client, cfg.MinioBucket, false)
	}))
	mux.HandleFunc("/view/", appmetrics.InstrumentHandlerFunc("downloader", "/view/{token}", func(w http.ResponseWriter, r *http.Request) {
		serveFile(w, r, redisClient, s3Client, cfg.MinioBucket, true)
	}))
	mux.HandleFunc("/health", appmetrics.InstrumentHandlerFunc("downloader", "/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))

	server := &http.Server{
		Addr:              ":" + cfg.HTTPPort,
		Handler:           mux,
		ReadHeaderTimeout: cfg.HTTPReadHeaderTimeout,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
		IdleTimeout:       cfg.HTTPIdleTimeout,
	}

	go func() {
		<-appCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Downloader shutdown failed: %v", err)
		}
	}()

	log.Printf("Downloader service started on :%s", cfg.HTTPPort)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func serveFile(w http.ResponseWriter, r *http.Request, rdb *redis.Client, s3c *s3.Client, bucket string, isInline bool) {
	route := "/download/{token}"
	if isInline {
		route = "/view/{token}"
	}
	signedLabel := "false"
	if r.URL.Query().Get("signed") == "1" {
		signedLabel = "true"
	}
	result := "error"
	defer func() {
		appmetrics.DownloadRequests.WithLabelValues(route, signedLabel, result).Inc()
	}()

	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 || parts[2] == "" {
		result = "bad_request"
		http.Error(w, "Token required", http.StatusBadRequest)
		return
	}
	token := parts[2]

	lookupStart := time.Now()
	depStart := time.Now()
	val, err := rdb.Get(r.Context(), "doc:"+token).Result()
	appmetrics.ObserveDependency("downloader", "redis", "redis_get", depStart, err)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			result = "not_found"
			appmetrics.DownloadLookupDuration.WithLabelValues(signedLabel, result).Observe(time.Since(lookupStart).Seconds())
			log.Printf("Token lookup failed for %s: %v", logutil.MaskToken(token), err)
			http.Error(w, "Link expired or invalid", http.StatusNotFound)
			return
		}
		appmetrics.DownloadLookupDuration.WithLabelValues(signedLabel, result).Observe(time.Since(lookupStart).Seconds())
		log.Printf("Token lookup failed for %s: %v", logutil.MaskToken(token), err)
		http.Error(w, "Metadata lookup failed", http.StatusInternalServerError)
		return
	}

	var meta FileMeta
	if err := json.Unmarshal([]byte(val), &meta); err != nil {
		appmetrics.DownloadLookupDuration.WithLabelValues(signedLabel, result).Observe(time.Since(lookupStart).Seconds())
		log.Printf("Invalid Redis metadata for %s: %v", logutil.MaskToken(token), err)
		http.Error(w, "Invalid file metadata", http.StatusInternalServerError)
		return
	}
	if meta.S3Key == "" {
		appmetrics.DownloadLookupDuration.WithLabelValues(signedLabel, result).Observe(time.Since(lookupStart).Seconds())
		http.Error(w, "Invalid file metadata", http.StatusInternalServerError)
		return
	}

	if r.URL.Query().Get("signed") == "1" {
		if db == nil {
			appmetrics.DownloadLookupDuration.WithLabelValues(signedLabel, result).Observe(time.Since(lookupStart).Seconds())
			http.Error(w, "Signed mode unavailable", http.StatusInternalServerError)
			return
		}

		var s SigningSession
		depStart = time.Now()
		res := db.WithContext(r.Context()).First(&s, "token = ?", token)
		appmetrics.ObserveDependency("downloader", "postgres", "signed_lookup", depStart, res.Error)
		if errors.Is(res.Error, gorm.ErrRecordNotFound) || s.SignedS3Key == "" {
			result = "not_found"
			appmetrics.SignedLookupMissing.Inc()
			appmetrics.DownloadLookupDuration.WithLabelValues(signedLabel, result).Observe(time.Since(lookupStart).Seconds())
			log.Printf("Signed lookup failed for %s: %v", logutil.MaskToken(token), res.Error)
			http.Error(w, "Signed document not found", http.StatusNotFound)
			return
		}
		if res.Error != nil {
			appmetrics.DownloadLookupDuration.WithLabelValues(signedLabel, result).Observe(time.Since(lookupStart).Seconds())
			http.Error(w, "DB error", http.StatusInternalServerError)
			return
		}
		meta.S3Key = s.SignedS3Key
		meta.OriginalName = "signed_" + meta.OriginalName
	}
	appmetrics.DownloadLookupDuration.WithLabelValues(signedLabel, "success").Observe(time.Since(lookupStart).Seconds())

	depStart = time.Now()
	obj, err := s3c.GetObject(r.Context(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(meta.S3Key),
	})
	appmetrics.ObserveDependency("downloader", "minio", "s3_get", depStart, err)
	if err != nil {
		appmetrics.DownloadS3Read.WithLabelValues(signedLabel, "error").Inc()
		log.Printf("S3 lookup failed for token=%s key=%s: %v", logutil.MaskToken(token), meta.S3Key, err)
		http.Error(w, "File storage error", http.StatusInternalServerError)
		return
	}
	defer obj.Body.Close()

	dispositionType := "attachment"
	if isInline {
		dispositionType = "inline"
	}

	filename := sanitizedFilename(meta.OriginalName)
	w.Header().Set("Content-Disposition", mustFormatContentDisposition(dispositionType, filename))
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	n, err := io.Copy(w, obj.Body)
	if n > 0 {
		appmetrics.DownloadS3ReadBytes.WithLabelValues(signedLabel).Observe(float64(n))
	}
	if err != nil {
		appmetrics.DownloadS3Read.WithLabelValues(signedLabel, "error").Inc()
		log.Printf("Stream error for token=%s key=%s: %v", logutil.MaskToken(token), meta.S3Key, err)
		return
	}
	appmetrics.DownloadS3Read.WithLabelValues(signedLabel, "success").Inc()
	result = "success"
}

func sanitizedFilename(name string) string {
	name = strings.TrimSpace(filepath.Base(name))
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "document.pdf"
	}

	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '\r' || r == '\n':
			continue
		case unicode.IsControl(r):
			continue
		case r == '/' || r == '\\':
			continue
		default:
			b.WriteRune(r)
		}
	}

	safe := strings.TrimSpace(b.String())
	if safe == "" {
		return "document.pdf"
	}
	if !strings.HasSuffix(strings.ToLower(safe), ".pdf") {
		safe += ".pdf"
	}
	return safe
}

func mustFormatContentDisposition(dispositionType, filename string) string {
	value := mime.FormatMediaType(dispositionType, map[string]string{"filename": filename})
	if value == "" {
		return dispositionType + `; filename="document.pdf"`
	}
	return value
}
