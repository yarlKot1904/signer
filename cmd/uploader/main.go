package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/yarlKot1904/signer/internal/config"
	"github.com/yarlKot1904/signer/internal/infra"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/s3store"
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

	store := s3store.New(cfg.MinioBucket, s3Client)
	composer := handler.NewStoreComposer()
	store.UseIn(composer)

	tusHandler, err := handler.NewHandler(handler.Config{
		BasePath:              "/files/",
		StoreComposer:         composer,
		NotifyCompleteUploads: true,
	})
	if err != nil {
		log.Fatal("Tusd handler error:", err)
	}

	go func() {
		for {
			event := <-tusHandler.CompleteUploads
			handleUploadComplete(event, s3Client, cfg.MinioBucket, redisClient)
		}
	}()

	http.Handle("/files/", http.StripPrefix("/files/", tusHandler))

	http.Handle("/", http.FileServer(http.Dir("./static")))

	log.Printf("Uploader service started on :%s", cfg.HTTPPort)
	if err := http.ListenAndServe(":"+cfg.HTTPPort, nil); err != nil {
		log.Fatal(err)
	}
}

func handleUploadComplete(event handler.HookEvent, s3Client *s3.Client, bucket string, rdb *redis.Client) {
	email := event.Upload.MetaData["userEmail"]
	filename := event.Upload.MetaData["filename"]
	if filename == "" {
		filename = "document.pdf"
	}
	now := time.Now()
	oldKey := event.Upload.Storage["Key"]
	newKey := fmt.Sprintf("%d/%02d/%s", now.Year(), int(now.Month()), oldKey)

	ctx := context.Background()
	err := infra.MoveObject(ctx, s3Client, bucket, oldKey, newKey)
	finalKey := oldKey
	if err != nil {
		log.Printf("Error moving object in S3: %v", err)
		return
	} else {
		finalKey = newKey
		log.Printf("Moved object in S3 from %s to %s", oldKey, newKey)
	}
	oldInfoKey := oldKey + ".info"
	newInfoKey := newKey + ".info"

	errInfo := infra.MoveObject(ctx, s3Client, bucket, oldInfoKey, newInfoKey)
	if errInfo != nil {
		log.Printf("Warning: could not move .info file: %v", errInfo)
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
	}
	taskJSON, _ := json.Marshal(task)

	err = rdb.Publish(context.Background(), "generation-tasks", taskJSON).Err()
	if err != nil {
		log.Printf("Failed to publish task: %v", err)
	} else {
		log.Printf("Published generation task for: %s", downloadToken)
	}

	log.Printf("File: %s (%s)", filename, email)
	log.Printf("Download: http://signer.local/download/%s", downloadToken)
	log.Printf("View: http://signer.local/view/%s", downloadToken)
}
