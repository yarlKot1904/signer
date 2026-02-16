package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/yarlKot1904/signer/internal/config"
	"github.com/yarlKot1904/signer/internal/infra"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

const MaxAttempts = 3

type SigningSession struct {
	Token    string `gorm:"primaryKey"`
	Email    string `gorm:"not null"`
	CodeHash string `gorm:"not null"`
	S3Key    string `gorm:"not null"`

	IsUsed    bool      `gorm:"default:false"`
	CreatedAt time.Time `gorm:"autoCreateTime"`
	Attempts  int       `gorm:"default:0"`

	EncryptedPrivKey string
	CertPEM          string

	SignedS3Key string
	SignedAt    *time.Time
}

type TaskMessage struct {
	Token string `json:"token"`
	Email string `json:"email"`
	S3Key string `json:"s3_key"`
}

type SignRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

var (
	db        *gorm.DB
	appCfg    *config.Config
	appCtx    context.Context
	s3Client  *s3.Client
	masterKey []byte
)

func main() {
	var err error

	appCfg, err = config.Load()
	if err != nil {
		log.Fatal(err)
	}

	appCtx = context.Background()

	if appCfg.PDFSignURL == "" {
		log.Fatal("PDFSIGN_URL is required")
	}

	masterKey, err = decodeMasterKey(appCfg.MasterKeyHex)
	if err != nil {
		log.Fatal("MASTER_KEY_HEX error:", err)
	}

	s3Client, err = infra.NewS3Client(appCtx, appCfg.MinioEndpoint, appCfg.MinioID, appCfg.MinioSecret, appCfg.MinioRegion)
	if err != nil {
		log.Fatal("S3 connect failed:", err)
	}

	db, err = gorm.Open(postgres.Open(appCfg.DBDSN), &gorm.Config{})
	if err != nil {
		log.Fatal("Unable to connect to database:", err)
	}

	log.Println("Running auto-migrations...")
	if err := db.AutoMigrate(&SigningSession{}); err != nil {
		log.Fatal("Migration failed:", err)
	}

	rabbitConn, err := amqp.Dial(appCfg.RabbitURL)
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
			processTask(appCtx, d.Body)
		}
	}()

	http.HandleFunc("/api/sign", handleSignRequest)

	log.Printf("Signer API started on :%s", appCfg.HTTPPort)
	if err := http.ListenAndServe(":"+appCfg.HTTPPort, nil); err != nil {
		log.Fatal(err)
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

	session := SigningSession{
		Token:    task.Token,
		Email:    task.Email,
		CodeHash: string(hash),
		S3Key:    task.S3Key,
	}

	if err := db.Create(&session).Error; err != nil {
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

	var session SigningSession
	result := db.First(&session, "token = ?", req.Token)

	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		http.Error(w, `{"error":"Session not found"}`, http.StatusNotFound)
		return
	} else if result.Error != nil {
		http.Error(w, `{"error":"Internal error"}`, http.StatusInternalServerError)
		return
	}

	if session.Attempts >= MaxAttempts {
		http.Error(w, `{"error":"Too many attempts. Session blocked."}`, http.StatusForbidden)
		return
	}
	if session.IsUsed {
		http.Error(w, `{"error":"Document already signed"}`, http.StatusForbidden)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(session.CodeHash), []byte(req.Password)); err != nil {
		session.Attempts++
		_ = db.Save(&session).Error

		msg := fmt.Sprintf(`{"error":"Invalid code. Attempts remaining: %d"}`, MaxAttempts-session.Attempts)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(msg))
		return
	}

	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		http.Error(w, `{"error":"Key gen failed"}`, http.StatusInternalServerError)
		return
	}

	certPEM, keyPEM, err := generateSelfSignedCertPEM(session.Email, privKey)
	if err != nil {
		http.Error(w, `{"error":"Cert gen failed"}`, http.StatusInternalServerError)
		return
	}

	encryptedPrivKey, err := encryptAES(masterKey, keyPEM)
	if err != nil {
		http.Error(w, `{"error":"Encryption failed"}`, http.StatusInternalServerError)
		return
	}

	pdfBytes, err := getObjectBytes(appCtx, s3Client, appCfg.MinioBucket, session.S3Key)
	if err != nil {
		http.Error(w, `{"error":"Failed to load original PDF"}`, http.StatusInternalServerError)
		return
	}

	signedPdf, err := signPDFViaService(appCfg.PDFSignURL, pdfBytes, certPEM, keyPEM)
	if err != nil {
		log.Printf("pdfsigner error: %v", err)
		http.Error(w, `{"error":"PDF signing failed"}`, http.StatusInternalServerError)
		return
	}

	signedKey := "signed/" + session.S3Key
	if err := putObjectBytes(appCtx, s3Client, appCfg.MinioBucket, signedKey, signedPdf, "application/pdf"); err != nil {
		http.Error(w, `{"error":"Failed to store signed PDF"}`, http.StatusInternalServerError)
		return
	}

	now := time.Now()
	session.IsUsed = true
	session.EncryptedPrivKey = encryptedPrivKey
	session.CertPEM = string(certPEM)
	session.SignedS3Key = signedKey
	session.SignedAt = &now
	_ = db.Save(&session).Error

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(fmt.Sprintf(`{"status":"success","signed_url":"/download/%s?signed=1"}`, session.Token)))
	log.Printf("SIGNED URL: http://signer.local/download/%s?signed=1", session.Token)
}

func generateCode() (string, error) {
	max := big.NewInt(1000000)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func decodeMasterKey(hexStr string) ([]byte, error) {
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes (64 hex chars), got %d bytes", len(b))
	}
	return b, nil
}

func generateSelfSignedCertPEM(email string, priv *rsa.PrivateKey) ([]byte, []byte, error) {
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName:   email,
			Organization: []string{"CryptoSigner Demo"},
		},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageEmailProtection},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})

	return certPEM, keyPEM, nil
}

func signPDFViaService(pdfSignURL string, pdfBytes, certPEM, keyPEM []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	pdfHdr := make(textproto.MIMEHeader)
	pdfHdr.Set("Content-Disposition", `form-data; name="pdf"; filename="document.pdf"`)
	pdfHdr.Set("Content-Type", "application/pdf")

	pdfPart, err := w.CreatePart(pdfHdr)
	if err != nil {
		return nil, err
	}
	if _, err := pdfPart.Write(pdfBytes); err != nil {
		return nil, err
	}

	if err := w.WriteField("certPem", string(certPEM)); err != nil {
		return nil, err
	}
	if err := w.WriteField("keyPem", string(keyPEM)); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, pdfSignURL, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pdfsigner error: %s %s", resp.Status, string(b))
	}

	return io.ReadAll(resp.Body)
}

func getObjectBytes(ctx context.Context, s3c *s3.Client, bucket, key string) ([]byte, error) {
	obj, err := s3c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer obj.Body.Close()
	return io.ReadAll(obj.Body)
}

func putObjectBytes(ctx context.Context, s3c *s3.Client, bucket, key string, data []byte, contentType string) error {
	_, err := s3c.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType),
	})
	return err
}

func encryptAES(key, data []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, aesGCM.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := aesGCM.Seal(nonce, nonce, data, nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}
