package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/yarlKot1904/signer/internal/config"
	"github.com/yarlKot1904/signer/internal/infra"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/redis/go-redis/v9"
)

type FileMeta struct {
	OriginalName string `json:"original_name"`
	S3Key        string `json:"s3_key"`
	MimeType     string `json:"mime_type"`
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("Config error:", err)
	}

	ctx := context.Background()
	s3Client, err := infra.NewS3Client(ctx, cfg.MinioEndpoint, cfg.MinioID, cfg.MinioSecret, cfg.MinioRegion)
	if err != nil {
		log.Fatal("S3 connect failed:", err)
	}

	redisClient, err := infra.NewRedisClient(cfg.RedisAddr)
	if err != nil {
		log.Fatal("Redis connect failed:", err)
	}

	http.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		serveFile(w, r, ctx, redisClient, s3Client, cfg.MinioBucket, false)
	})

	http.HandleFunc("/view/", func(w http.ResponseWriter, r *http.Request) {
		serveFile(w, r, ctx, redisClient, s3Client, cfg.MinioBucket, true)
	})

	log.Printf("📥 Downloader (v2) service started on :%s", cfg.HTTPPort)
	if err := http.ListenAndServe(":"+cfg.HTTPPort, nil); err != nil {
		log.Fatal(err)
	}
}

func serveFile(w http.ResponseWriter, r *http.Request, ctx context.Context, rdb *redis.Client, s3c *s3.Client, bucket string, isInline bool) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 || parts[2] == "" {
		http.Error(w, "Token required", http.StatusBadRequest)
		return
	}
	token := parts[2]

	val, err := rdb.Get(ctx, "doc:"+token).Result()
	if err != nil {
		http.Error(w, "Link expired or invalid", http.StatusNotFound)
		return
	}

	var meta FileMeta
	json.Unmarshal([]byte(val), &meta)

	obj, err := s3c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(meta.S3Key),
	})
	if err != nil {
		log.Printf("S3 error: %v", err)
		http.Error(w, "File storage error", http.StatusInternalServerError)
		return
	}
	defer obj.Body.Close()

	dispositionType := "attachment"
	if isInline {
		dispositionType = "inline"
	}

	w.Header().Set("Content-Disposition", dispositionType+"; filename=\""+meta.OriginalName+"\"")
	w.Header().Set("Content-Type", meta.MimeType)

	if _, err := io.Copy(w, obj.Body); err != nil {
		log.Println("Stream error:", err)
	}
}
