package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/yarlKot1904/signer/internal/config"
	"github.com/yarlKot1904/signer/internal/infra"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/s3store"

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

	ctx := context.Background()

	s3Client, err := infra.NewS3Client(ctx, cfg.MinioEndpoint, cfg.MinioID, cfg.MinioSecret, cfg.MinioRegion)
	if err != nil {
		log.Fatal("S3 connect failed:", err)
	}

	redisClient, err := infra.NewRedisClient(cfg.RedisAddr)
	if err != nil {
		log.Fatal("Redis connect failed:", err)
	}

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
	})
	if err != nil {
		log.Fatal("Tusd handler error:", err)
	}

	verifyTusHandler, err := handler.NewHandler(handler.Config{
		BasePath:              "/verify-files/",
		StoreComposer:         composer,
		NotifyCompleteUploads: true,
	})
	if err != nil {
		log.Fatal("Verify tusd handler error:", err)
	}

	go func() {
		for {
			event := <-tusHandler.CompleteUploads
			handleUploadComplete(event, s3Client, cfg.MinioBucket, redisClient, rabbitCh, q.Name)
		}
	}()

	go func() {
		for {
			event := <-verifyTusHandler.CompleteUploads
			handleVerifyUploadComplete(event, s3Client, cfg.MinioBucket, redisClient)
		}
	}()

	go runVerifyCleanupLoop(ctx, s3Client, cfg.MinioBucket, redisClient)

	http.Handle("/files/", http.StripPrefix("/files/", tusHandler))
	http.Handle("/verify-files/", http.StripPrefix("/verify-files/", verifyTusHandler))

	http.Handle("/", http.FileServer(http.Dir("./static")))

	log.Printf("Uploader service started on :%s", cfg.HTTPPort)
	if err := http.ListenAndServe(":"+cfg.HTTPPort, nil); err != nil {
		log.Fatal(err)
	}
}

func handleUploadComplete(event handler.HookEvent, s3Client *s3.Client, bucket string, rdb *redis.Client, rabbitCh *amqp.Channel, queueName string) {
	email := event.Upload.MetaData["userEmail"]
	filename := event.Upload.MetaData["filename"]
	if filename == "" {
		filename = "document.pdf"
	}

	ctx := context.Background()
	finalKey, err := moveUploadedObject(ctx, s3Client, bucket, event.Upload.Storage["Key"], "")
	if err != nil {
		log.Printf("Error moving signing upload in S3: %v", err)
		return
	}

	downloadToken := uuid.New().String()

	meta := FileMeta{
		OriginalName: filename,
		S3Key:        finalKey,
		MimeType:     event.Upload.MetaData["filetype"],
		OwnerEmail:   email,
	}

	data, _ := json.Marshal(meta)

	err = rdb.Set(ctx, "doc:"+downloadToken, data, 24*time.Hour).Err()
	if err != nil {
		log.Printf("Error saving to Redis: %v", err)
		return
	}
	task := TaskMessage{
		Token: downloadToken,
		Email: email,
		S3Key: finalKey,
	}
	taskJSON, _ := json.Marshal(task)

	err = rabbitCh.PublishWithContext(ctx,
		"",
		queueName,
		false,
		false,
		amqp.Publishing{
			ContentType: "application/json",
			Body:        taskJSON,
		})

	if err != nil {
		log.Printf("Failed to publish task: %v", err)
	} else {
		log.Printf("Published task to RabbitMQ for: %s", downloadToken)
	}

	log.Printf("File: %s (%s)", filename, email)
	log.Printf("Download: http://signer.local/download/%s", downloadToken)
	log.Printf("View: http://signer.local/view/%s", downloadToken)
	log.Printf("Sign: http://signer.local/sign.html?token=%s", downloadToken)
}

func handleVerifyUploadComplete(event handler.HookEvent, s3Client *s3.Client, bucket string, rdb *redis.Client) {
	filename := event.Upload.MetaData["filename"]
	if filename == "" {
		filename = "document.pdf"
	}

	ctx := context.Background()
	finalKey, err := moveUploadedObject(ctx, s3Client, bucket, event.Upload.Storage["Key"], verifyObjectPrefix)
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
		MimeType:     event.Upload.MetaData["filetype"],
	}

	data, _ := json.Marshal(meta)
	if err := rdb.Set(ctx, "verify:"+verifyToken, data, verifyUploadTTL).Err(); err != nil {
		log.Printf("Error saving verify upload metadata to Redis: %v", err)
		return
	}
	if err := rdb.ZAdd(ctx, verifyCleanupZSetKey, redis.Z{
		Score:  float64(time.Now().Add(verifyUploadTTL).Unix()),
		Member: finalKey,
	}).Err(); err != nil {
		log.Printf("Warning: could not schedule verify cleanup for %s: %v", finalKey, err)
	}

	log.Printf("Stored verify upload: token=%s key=%s", verifyToken, finalKey)
}

func moveUploadedObject(ctx context.Context, s3Client *s3.Client, bucket, oldKey, keyPrefix string) (string, error) {
	now := time.Now()
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

func runVerifyCleanupLoop(ctx context.Context, s3Client *s3.Client, bucket string, rdb *redis.Client) {
	ticker := time.NewTicker(verifyCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cleanupExpiredVerifyObjects(ctx, s3Client, bucket, rdb)
		case <-ctx.Done():
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
