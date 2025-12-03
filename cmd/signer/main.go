package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/yarlKot1904/signer/internal/config"
	"github.com/yarlKot1904/signer/internal/infra"
)

type TaskMessage struct {
	Token string `json:"token"`
	Email string `json:"email"`
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	rdb, err := infra.NewRedisClient(cfg.RedisAddr)
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()

	topic := "generation-tasks"
	pubsub := rdb.Subscribe(ctx, topic)
	defer pubsub.Close()

	if _, err := pubsub.Receive(ctx); err != nil {
		log.Fatal("Subscribe error:", err)
	}

	log.Printf("Signer service started. Listening on channel: %s", topic)

	ch := pubsub.Channel()

	for msg := range ch {
		log.Printf("Received task: %s", msg.Payload)

		var task TaskMessage
		if err := json.Unmarshal([]byte(msg.Payload), &task); err != nil {
			log.Printf("Bad JSON: %v", err)
			continue
		}

		err := generateAndSaveKeys(ctx, rdb, task.Token)
		if err != nil {
			log.Fatalf("Key generation failed: %v", err)
		} else {
			log.Printf("Keys generated for session: %s", task.Token)
		}
	}
}

func generateAndSaveKeys(ctx context.Context, rdb *redis.Client, token string) error {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	privBytes := x509.MarshalPKCS1PrivateKey(privateKey)
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: privBytes,
	})

	pubBytes := x509.MarshalPKCS1PublicKey(&privateKey.PublicKey)
	pubPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PUBLIC KEY",
		Bytes: pubBytes,
	})

	pipe := rdb.Pipeline()
	pipe.Set(ctx, "session:"+token+":private", string(privPEM), 24*time.Hour)
	pipe.Set(ctx, "session:"+token+":public", string(pubPEM), 24*time.Hour)

	_, err = pipe.Exec(ctx)
	return err
}
