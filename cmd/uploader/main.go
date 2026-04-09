package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/s3store"
	"github.com/yarlKot1904/signer/internal/config"
	"github.com/yarlKot1904/signer/internal/infra"

	amqp "github.com/rabbitmq/amqp091-go"
)

type FileMeta struct {
	OriginalName string `json:"original_name"`
	S3Key        string `json:"s3_key"`
	MimeType     string `json:"mime_type"`
	OwnerEmail   string `json:"owner_email"`
}

type TaskMessage struct {
	Token string `json:"token"`
	Email string `json:"email"`
	S3Key string `json:"s3_key"`
}

const (
	verifyUploadTTL       = time.Hour
	verifyCleanupZSetKey  = "verify:cleanup"
	verifyCleanupInterval = time.Minute
	verifyObjectPrefix    = "verify/"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("Config error:", err)
	}

	appCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	store := s3store.New(cfg.MinioBucket, s3Client)
	composer := handler.NewStoreComposer()
	store.UseIn(composer)

	rabbitConn, err := amqp.Dial(cfg.RabbitURL)
	if err != nil {
		log.Fatal("RabbitMQ connect failed:", err)
	}
	defer rabbitConn.Close()

	rabbitCh, err := rabbitConn.Channel()
	if err != nil {
		log.Fatal("RabbitMQ channel failed:", err)
	}
	defer rabbitCh.Close()

	q, err := rabbitCh.QueueDeclare(
		"signer.tasks",
		true,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		log.Fatal(err)
	}

	tusHandler, err := handler.NewHandler(handler.Config{
		BasePath:              "/files/",
		StoreComposer:         composer,
		NotifyCompleteUploads: true,
		MaxSize:               cfg.UploadMaxBytes,
	})
	if err != nil {
		log.Fatal("Tusd handler error:", err)
	}

	verifyTusHandler, err := handler.NewHandler(handler.Config{
		BasePath:              "/verify-files/",
		StoreComposer:         composer,
		NotifyCompleteUploads: true,
		MaxSize:               cfg.UploadMaxBytes,
	})
	if err != nil {
		log.Fatal("Verify tusd handler error:", err)
	}

	go handleUploadLoop(appCtx, cfg, tusHandler.CompleteUploads, s3Client, redisClient, rabbitCh, q.Name)
	go handleVerifyUploadLoop(appCtx, cfg, verifyTusHandler.CompleteUploads, s3Client, redisClient)
	go runVerifyCleanupLoop(appCtx, cfg, s3Client, redisClient)

	mux := http.NewServeMux()
	mux.Handle("/files/", http.StripPrefix("/files/", tusHandler))
	mux.Handle("/verify-files/", http.StripPrefix("/verify-files/", verifyTusHandler))
	mux.Handle("/", http.FileServer(http.Dir("./static")))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

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
			log.Printf("Uploader shutdown failed: %v", err)
		}
	}()

	log.Printf("Uploader service started on :%s", cfg.HTTPPort)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func handleUploadLoop(
	appCtx context.Context,
	cfg *config.Config,
	events <-chan handler.HookEvent,
	s3Client *s3.Client,
	rdb *redis.Client,
	rabbitCh *amqp.Channel,
	queueName string,
) {
	for {
		select {
		case <-appCtx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			handleUploadComplete(appCtx, cfg, event, s3Client, cfg.MinioBucket, rdb, rabbitCh, queueName)
		}
	}
}

func handleVerifyUploadLoop(
	appCtx context.Context,
	cfg *config.Config,
	events <-chan handler.HookEvent,
	s3Client *s3.Client,
	rdb *redis.Client,
) {
	for {
		select {
		case <-appCtx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			handleVerifyUploadComplete(appCtx, cfg, event, s3Client, cfg.MinioBucket, rdb)
		}
	}
}

func handleUploadComplete(
	appCtx context.Context,
	cfg *config.Config,
	event handler.HookEvent,
	s3Client *s3.Client,
	bucket string,
	rdb *redis.Client,
	rabbitCh *amqp.Channel,
	queueName string,
) {
	storageKey := event.Upload.Storage["Key"]
	if storageKey == "" {
		log.Printf("Upload completed without storage key: uploadID=%s", event.Upload.ID)
		return
	}

	opCtx, cancel := context.WithTimeout(appCtx, cfg.DependencyTimeout)
	defer cancel()

	isPDF, err := isPDFObject(opCtx, s3Client, bucket, storageKey)
	if err != nil {
		log.Printf("Upload PDF validation failed for %s: %v", storageKey, err)
		return
	}
	if !isPDF {
		log.Printf("Rejected non-PDF upload: key=%s", storageKey)
		if err := deleteUploadArtifacts(opCtx, s3Client, bucket, storageKey); err != nil {
			log.Printf("Failed to delete rejected upload %s: %v", storageKey, err)
		}
		return
	}

	email := event.Upload.MetaData["userEmail"]
	filename := event.Upload.MetaData["filename"]
	if filename == "" {
		filename = "document.pdf"
	}

	finalKey, err := moveUploadedObject(opCtx, s3Client, bucket, storageKey, "")
	if err != nil {
		log.Printf("Error moving signing upload in S3: %v", err)
		return
	}

	downloadToken := uuid.New().String()
	meta := FileMeta{
		OriginalName: filename,
		S3Key:        finalKey,
		MimeType:     "application/pdf",
		OwnerEmail:   email,
	}

	data, err := json.Marshal(meta)
	if err != nil {
		log.Printf("Error marshaling upload metadata for %s: %v", finalKey, err)
		_ = deleteUploadArtifacts(opCtx, s3Client, bucket, finalKey)
		return
	}

	if err := rdb.Set(opCtx, "doc:"+downloadToken, data, 24*time.Hour).Err(); err != nil {
		log.Printf("Error saving to Redis: %v", err)
		_ = deleteUploadArtifacts(opCtx, s3Client, bucket, finalKey)
		return
	}

	task := TaskMessage{
		Token: downloadToken,
		Email: email,
		S3Key: finalKey,
	}
	taskJSON, err := json.Marshal(task)
	if err != nil {
		log.Printf("Failed to marshal task for %s: %v", downloadToken, err)
		_ = rdb.Del(opCtx, "doc:"+downloadToken).Err()
		_ = deleteUploadArtifacts(opCtx, s3Client, bucket, finalKey)
		return
	}

	err = rabbitCh.PublishWithContext(opCtx,
		"",
		queueName,
		false,
		false,
		amqp.Publishing{
			ContentType: "application/json",
			Body:        taskJSON,
		},
	)
	if err != nil {
		log.Printf("Failed to publish task: %v", err)
		_ = rdb.Del(opCtx, "doc:"+downloadToken).Err()
		_ = deleteUploadArtifacts(opCtx, s3Client, bucket, finalKey)
		return
	}

	log.Printf("Upload complete: file=%s email=%s finalKey=%s token=%s", filename, email, finalKey, downloadToken)
	log.Printf("Download: http://signer.local/download/%s", downloadToken)
	log.Printf("View: http://signer.local/view/%s", downloadToken)
	log.Printf("Sign: http://signer.local/sign.html?token=%s", downloadToken)
}

func handleVerifyUploadComplete(
	appCtx context.Context,
	cfg *config.Config,
	event handler.HookEvent,
	s3Client *s3.Client,
	bucket string,
	rdb *redis.Client,
) {
	storageKey := event.Upload.Storage["Key"]
	if storageKey == "" {
		log.Printf("Verify upload completed without storage key: uploadID=%s", event.Upload.ID)
		return
	}

	opCtx, cancel := context.WithTimeout(appCtx, cfg.DependencyTimeout)
	defer cancel()

	isPDF, err := isPDFObject(opCtx, s3Client, bucket, storageKey)
	if err != nil {
		log.Printf("Verify upload PDF validation failed for %s: %v", storageKey, err)
		return
	}
	if !isPDF {
		log.Printf("Rejected non-PDF verify upload: key=%s", storageKey)
		if err := deleteUploadArtifacts(opCtx, s3Client, bucket, storageKey); err != nil {
			log.Printf("Failed to delete rejected verify upload %s: %v", storageKey, err)
		}
		return
	}

	filename := event.Upload.MetaData["filename"]
	if filename == "" {
		filename = "document.pdf"
	}

	finalKey, err := moveUploadedObject(opCtx, s3Client, bucket, storageKey, verifyObjectPrefix)
	if err != nil {
		log.Printf("Error moving verify upload in S3: %v", err)
		return
	}

	verifyToken := event.Upload.MetaData["verifyToken"]
	if verifyToken == "" {
		verifyToken = uuid.New().String()
	}

	meta := FileMeta{
		OriginalName: filename,
		S3Key:        finalKey,
		MimeType:     "application/pdf",
	}

	data, err := json.Marshal(meta)
	if err != nil {
		log.Printf("Error marshaling verify metadata for %s: %v", finalKey, err)
		_ = deleteVerifyArtifacts(opCtx, s3Client, bucket, finalKey)
		return
	}
	if err := rdb.Set(opCtx, "verify:"+verifyToken, data, verifyUploadTTL).Err(); err != nil {
		log.Printf("Error saving verify upload metadata to Redis: %v", err)
		_ = deleteVerifyArtifacts(opCtx, s3Client, bucket, finalKey)
		return
	}
	if err := rdb.ZAdd(opCtx, verifyCleanupZSetKey, redis.Z{
		Score:  float64(time.Now().Add(verifyUploadTTL).Unix()),
		Member: finalKey,
	}).Err(); err != nil {
		log.Printf("Warning: could not schedule verify cleanup for %s: %v", finalKey, err)
	}

	log.Printf("Stored verify upload: token=%s key=%s", verifyToken, finalKey)
}

func moveUploadedObject(ctx context.Context, s3Client *s3.Client, bucket, oldKey, keyPrefix string) (string, error) {
	now := time.Now().UTC()
	newKey := fmt.Sprintf("%s%d/%02d/%s", keyPrefix, now.Year(), int(now.Month()), oldKey)

	if err := infra.MoveObject(ctx, s3Client, bucket, oldKey, newKey); err != nil {
		return "", err
	}
	log.Printf("Moved object in S3 from %s to %s", oldKey, newKey)

	oldInfoKey := oldKey + ".info"
	newInfoKey := newKey + ".info"
	if err := infra.MoveObject(ctx, s3Client, bucket, oldInfoKey, newInfoKey); err != nil {
		log.Printf("Warning: could not move .info file: %v", err)
	}

	return newKey, nil
}

func runVerifyCleanupLoop(
	appCtx context.Context,
	cfg *config.Config,
	s3Client *s3.Client,
	rdb *redis.Client,
) {
	ticker := time.NewTicker(verifyCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			opCtx, cancel := context.WithTimeout(appCtx, cfg.DependencyTimeout)
			cleanupExpiredVerifyObjects(opCtx, s3Client, cfg.MinioBucket, rdb)
			cancel()
		case <-appCtx.Done():
			return
		}
	}
}

func cleanupExpiredVerifyObjects(ctx context.Context, s3Client *s3.Client, bucket string, rdb *redis.Client) {
	now := strconv.FormatInt(time.Now().Unix(), 10)
	keys, err := rdb.ZRangeByScore(ctx, verifyCleanupZSetKey, &redis.ZRangeBy{
		Min: "-inf",
		Max: now,
	}).Result()
	if err != nil {
		log.Printf("Verify cleanup scan failed: %v", err)
		return
	}

	for _, key := range keys {
		if err := deleteVerifyArtifacts(ctx, s3Client, bucket, key); err != nil {
			log.Printf("Verify cleanup delete failed for %s: %v", key, err)
			continue
		}
		if err := rdb.ZRem(ctx, verifyCleanupZSetKey, key).Err(); err != nil {
			log.Printf("Verify cleanup zset remove failed for %s: %v", key, err)
		}
		log.Printf("Deleted expired verify object: %s", key)
	}
}

func isPDFObject(ctx context.Context, s3Client *s3.Client, bucket, key string) (bool, error) {
	obj, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Range:  aws.String("bytes=0-7"),
	})
	if err != nil {
		return false, err
	}
	defer obj.Body.Close()

	header, err := io.ReadAll(obj.Body)
	if err != nil {
		return false, err
	}
	return hasPDFHeader(header), nil
}

func hasPDFHeader(header []byte) bool {
	return bytes.HasPrefix(header, []byte("%PDF-"))
}

func deleteUploadArtifacts(ctx context.Context, s3Client *s3.Client, bucket, key string) error {
	if err := infra.DeleteObject(ctx, s3Client, bucket, key); err != nil {
		return err
	}
	infoKey := key + ".info"
	if err := infra.DeleteObject(ctx, s3Client, bucket, infoKey); err != nil {
		log.Printf("Upload cleanup .info delete skipped for %s: %v", infoKey, err)
	}
	return nil
}

func deleteVerifyArtifacts(ctx context.Context, s3Client *s3.Client, bucket, key string) error {
	if err := infra.DeleteObject(ctx, s3Client, bucket, key); err != nil {
		return err
	}

	infoKey := key + ".info"
	if err := infra.DeleteObject(ctx, s3Client, bucket, infoKey); err != nil {
		log.Printf("Verify cleanup .info delete skipped for %s: %v", infoKey, err)
	}

	return nil
}
