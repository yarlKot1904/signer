package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

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
			handleUploadComplete(event, redisClient)
		}
	}()

	http.Handle("/files/", http.StripPrefix("/files/", tusHandler))

	http.Handle("/", http.FileServer(http.Dir("./static")))

	log.Printf("🚀 Uploader service started on :%s", cfg.HTTPPort)
	if err := http.ListenAndServe(":"+cfg.HTTPPort, nil); err != nil {
		log.Fatal(err)
	}
}

func handleUploadComplete(event handler.HookEvent, rdb *redis.Client) {
	email := event.Upload.MetaData["userEmail"]
	filename := event.Upload.MetaData["filename"]
	if filename == "" {
		filename = "document.pdf"
	}

	downloadToken := uuid.New().String()

	meta := FileMeta{
		OriginalName: filename,
		S3Key:        event.Upload.Storage["Key"],
		MimeType:     event.Upload.MetaData["filetype"],
		OwnerEmail:   email,
	}

	data, _ := json.Marshal(meta)

	ctx := context.Background()
	err := rdb.Set(ctx, "doc:"+downloadToken, data, 24*time.Hour).Err()
	if err != nil {
		log.Printf("Error saving to Redis: %v", err)
		return
	}

	log.Printf("File: %s (%s)", filename, email)
	log.Printf("Download: http://localhost:8081/download/%s", downloadToken)
	log.Printf("View: http://localhost:8081/view/%s", downloadToken)
}
