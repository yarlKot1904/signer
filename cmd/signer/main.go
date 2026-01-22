package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/yarlKot1904/signer/internal/config"
	"golang.org/x/crypto/bcrypt"
)

type TaskMessage struct {
	Token string `json:"token"`
	Email string `json:"email"`
	S3Key string `json:"s3_key"`
}

type SignRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

var db *pgxpool.Pool

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	db, err = pgxpool.New(ctx, cfg.DBDSN)
	if err != nil {
		log.Fatal("Unable to connect to database:", err)
	}
	defer db.Close()

	initDB(ctx, db)

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

	msgs, err := rabbitCh.Consume(
		q.Name,
		"",
		true,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		log.Printf("Signer Worker started. Waiting for messages.")
		for d := range msgs {
			processTask(ctx, d.Body)
		}
	}()

	http.HandleFunc("/api/sign", handleSignRequest)

	log.Printf("Signer API started on :%s", cfg.HTTPPort)
	if err := http.ListenAndServe(":"+cfg.HTTPPort, nil); err != nil {
		log.Fatal(err)
	}
}

func initDB(ctx context.Context, db *pgxpool.Pool) {
	query := `
	CREATE TABLE IF NOT EXISTS signing_sessions (
		token TEXT PRIMARY KEY,
		email TEXT NOT NULL,
		code_hash TEXT NOT NULL,
		s3_key TEXT NOT NULL,
		is_used BOOLEAN DEFAULT FALSE,
		created_at TIMESTAMP DEFAULT NOW()
	);
	`
	_, err := db.Exec(ctx, query)
	if err != nil {
		log.Fatalf("Migration failed: %v", err)
	}
}

func processTask(ctx context.Context, body []byte) {
	var task TaskMessage
	if err := json.Unmarshal(body, &task); err != nil {
		log.Printf("Bad JSON: %v", err)
		return
	}

	code, err := generateCode()
	if err != nil {
		log.Printf("Random error: %v", err)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("Hash error: %v", err)
		return
	}

	query := `
		INSERT INTO signing_sessions (token, email, code_hash, s3_key) 
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (token) DO NOTHING
	`
	_, err = db.Exec(ctx, query, task.Token, task.Email, string(hash), task.S3Key)
	if err != nil {
		log.Printf("DB Error: %v", err)
		return
	}

	log.Printf("==========================================")
	log.Printf("NEW TASK for %s", task.Email)
	log.Printf("OTP CODE: %s", code)
	log.Printf("TOKEN: %s", task.Token)
	log.Printf("==========================================")
}

func handleSignRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req SignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	var hash string
	var isUsed bool

	err := db.QueryRow(context.Background(),
		"SELECT code_hash, is_used FROM signing_sessions WHERE token=$1", req.Token).Scan(&hash, &isUsed)

	if err == pgx.ErrNoRows {
		http.Error(w, `{"error": "Session not found or expired"}`, http.StatusNotFound)
		return
	} else if err != nil {
		log.Printf("DB Query Error: %v", err)
		http.Error(w, `{"error": "Internal error"}`, http.StatusInternalServerError)
		return
	}

	if isUsed {
		http.Error(w, `{"error": "Code already used"}`, http.StatusForbidden)
		return
	}

	err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password))
	if err != nil {
		http.Error(w, `{"error": "Invalid code"}`, http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "success", "message": "Code valid. Signing logic pending..."}`))
}

func generateCode() (string, error) {
	max := big.NewInt(1000000)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}
