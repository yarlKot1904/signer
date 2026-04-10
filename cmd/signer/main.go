package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
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
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"github.com/yarlKot1904/signer/internal/config"
	"github.com/yarlKot1904/signer/internal/infra"
	"github.com/yarlKot1904/signer/internal/logutil"
	"github.com/yarlKot1904/signer/internal/mailer"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

	SignedS3Key        string
	SignedAt           *time.Time
	NotificationSentAt *time.Time
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

type VerifyRequest struct {
	Token       string `json:"token"`
	UploadToken string `json:"upload_token"`
}

type VerificationResult struct {
	Status                string  `json:"status"`
	ServiceOwned          bool    `json:"service_owned"`
	SignaturePresent      bool    `json:"signature_present"`
	IntegrityValid        bool    `json:"integrity_valid"`
	SignerSubject         *string `json:"signer_subject"`
	SignerCN              *string `json:"signer_cn"`
	SigningTime           *string `json:"signing_time"`
	CertificateSelfSigned *bool   `json:"certificate_self_signed"`
	CertificateSHA256     *string `json:"certificate_sha256"`
	CertificateTrusted    *bool   `json:"certificate_trusted"`
	Error                 *string `json:"error"`
}

type FileMeta struct {
	OriginalName string `json:"original_name"`
	S3Key        string `json:"s3_key"`
	MimeType     string `json:"mime_type"`
	OwnerEmail   string `json:"owner_email,omitempty"`
}

type SignedDocument struct {
	ID            uint      `gorm:"primaryKey"`
	Token         string    `gorm:"index"`
	SignedS3Key   string    `gorm:"uniqueIndex;not null"`
	SignedPDFSHA  string    `gorm:"column:signed_pdfsha;uniqueIndex;not null"`
	CertSHA       string    `gorm:"not null"`
	SignerSubject string    `gorm:"not null"`
	SignedAt      time.Time `gorm:"not null"`
	CreatedAt     time.Time `gorm:"autoCreateTime"`
}

type apiError struct {
	Status  int
	Message string
}

func (e apiError) Error() string {
	return e.Message
}

type taskAction int

const (
	taskAck taskAction = iota
	taskReject
	taskNackRequeue
)

const verifyCleanupZSetKey = "verify:cleanup"

var errNotificationAlreadySent = errors.New("notification already sent")

var (
	db         *gorm.DB
	appCfg     *config.Config
	s3Client   *s3.Client
	redisDB    *redis.Client
	masterKey  []byte
	httpClient *http.Client
)

func main() {
	var err error

	appCfg, err = config.Load()
	if err != nil {
		log.Fatal(err)
	}

	if appCfg.PDFSignURL == "" {
		log.Fatal("PDFSIGN_URL is required")
	}
	if appCfg.MailerURL == "" {
		log.Fatal("MAILER_URL is required")
	}

	masterKey, err = decodeMasterKey(appCfg.MasterKeyHex)
	if err != nil {
		log.Fatal("MASTER_KEY_HEX error:", err)
	}

	appCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpClient = &http.Client{
		Timeout: appCfg.PDFSignTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}

	s3Client, err = infra.NewS3Client(appCtx, appCfg.MinioEndpoint, appCfg.MinioID, appCfg.MinioSecret, appCfg.MinioRegion)
	if err != nil {
		log.Fatal("S3 connect failed:", err)
	}

	redisDB, err = infra.NewRedisClient(appCfg.RedisAddr)
	if err != nil {
		log.Fatal("Redis connect failed:", err)
	}
	defer func() {
		if err := redisDB.Close(); err != nil {
			log.Printf("Redis close failed: %v", err)
		}
	}()

	db, err = gorm.Open(postgres.Open(appCfg.DBDSN), &gorm.Config{})
	if err != nil {
		log.Fatal("Unable to connect to database:", err)
	}

	log.Println("Running auto-migrations...")
	if err := db.AutoMigrate(&SigningSession{}, &SignedDocument{}); err != nil {
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
		false,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		log.Fatal(err)
	}

	go consumeTasks(appCtx, msgs)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/sign", handleSignRequest)
	mux.HandleFunc("/api/verify", handleVerifyRequest)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	server := &http.Server{
		Addr:              ":" + appCfg.HTTPPort,
		Handler:           mux,
		ReadHeaderTimeout: appCfg.HTTPReadHeaderTimeout,
		ReadTimeout:       appCfg.HTTPReadTimeout,
		WriteTimeout:      appCfg.HTTPWriteTimeout,
		IdleTimeout:       appCfg.HTTPIdleTimeout,
	}

	go func() {
		<-appCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), appCfg.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Signer shutdown failed: %v", err)
		}
	}()

	log.Printf("Signer API started on :%s", appCfg.HTTPPort)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func consumeTasks(appCtx context.Context, msgs <-chan amqp.Delivery) {
	log.Printf("Signer Worker started. Waiting for messages.")

	for {
		select {
		case <-appCtx.Done():
			return
		case delivery, ok := <-msgs:
			if !ok {
				return
			}

			taskCtx, cancel := context.WithTimeout(appCtx, appCfg.DependencyTimeout)
			action := processTask(taskCtx, delivery.Body)
			cancel()

			switch action {
			case taskAck:
				if err := delivery.Ack(false); err != nil {
					log.Printf("RabbitMQ ack failed: %v", err)
				}
			case taskReject:
				if err := delivery.Reject(false); err != nil {
					log.Printf("RabbitMQ reject failed: %v", err)
				}
			case taskNackRequeue:
				if err := delivery.Nack(false, true); err != nil {
					log.Printf("RabbitMQ nack failed: %v", err)
				}
			}
		}
	}
}

func processTask(ctx context.Context, body []byte) taskAction {
	var task TaskMessage
	if err := json.Unmarshal(body, &task); err != nil {
		log.Printf("Bad task JSON: %v", err)
		return taskReject
	}
	if strings.TrimSpace(task.Token) == "" || strings.TrimSpace(task.Email) == "" || strings.TrimSpace(task.S3Key) == "" {
		log.Printf("Invalid task payload: token=%q email=%q s3_key=%q", logutil.MaskToken(task.Token), logutil.MaskEmail(task.Email), task.S3Key)
		return taskReject
	}

	code, err := generateCode()
	if err != nil {
		log.Printf("OTP generation error: %v", err)
		return taskNackRequeue
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("Hash error: %v", err)
		return taskNackRequeue
	}

	if err := upsertPendingNotification(ctx, task, string(hash)); err != nil {
		if errors.Is(err, errNotificationAlreadySent) {
			log.Printf("Duplicate task ignored for token=%s", logutil.MaskToken(task.Token))
			return taskAck
		}
		log.Printf("DB Error: %v", err)
		return taskNackRequeue
	}

	if err := notifyMailer(ctx, buildSigningNotification(task, code)); err != nil {
		log.Printf("Mailer dispatch failed for token=%s: %v", logutil.MaskToken(task.Token), err)
		return taskNackRequeue
	}

	now := time.Now().UTC()
	result := db.WithContext(ctx).
		Model(&SigningSession{}).
		Where("token = ? AND notification_sent_at IS NULL", task.Token).
		Update("notification_sent_at", &now)
	if result.Error != nil {
		log.Printf("Notification state update failed for token=%s: %v", logutil.MaskToken(task.Token), result.Error)
		return taskNackRequeue
	}
	if result.RowsAffected == 0 {
		log.Printf("Notification state already updated for token=%s", logutil.MaskToken(task.Token))
		return taskAck
	}

	log.Printf("Signing session prepared: token=%s recipient=%s notification=queued", logutil.MaskToken(task.Token), logutil.MaskEmail(task.Email))
	return taskAck
}

func upsertPendingNotification(ctx context.Context, task TaskMessage, codeHash string) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session SigningSession
		result := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&session, "token = ?", task.Token)
		switch {
		case errors.Is(result.Error, gorm.ErrRecordNotFound):
			session = SigningSession{
				Token:    task.Token,
				Email:    task.Email,
				CodeHash: codeHash,
				S3Key:    task.S3Key,
			}
			return tx.Create(&session).Error
		case result.Error != nil:
			return result.Error
		case session.NotificationSentAt != nil:
			return errNotificationAlreadySent
		default:
			session.Email = task.Email
			session.S3Key = task.S3Key
			session.CodeHash = codeHash
			session.Attempts = 0
			return tx.Save(&session).Error
		}
	})
}

func buildSigningNotification(task TaskMessage, code string) mailer.SendRequest {
	token := url.PathEscape(task.Token)
	signQuery := url.QueryEscape(task.Token)

	return mailer.SendRequest{
		Template:    mailer.TemplateSigningOTP,
		Recipient:   task.Email,
		MessageID:   task.Token,
		Correlation: task.Token,
		Variables: map[string]string{
			"code":         code,
			"sign_url":     joinPublicURL(appCfg.PublicBaseURL, "/sign.html?token="+signQuery),
			"download_url": joinPublicURL(appCfg.PublicBaseURL, "/download/"+token),
			"view_url":     joinPublicURL(appCfg.PublicBaseURL, "/view/"+token),
		},
	}
}

func buildSignedDocumentNotification(recipient, token string) mailer.SendRequest {
	escapedToken := url.PathEscape(token)
	signedDownloadURL := joinPublicURL(appCfg.PublicBaseURL, "/download/"+escapedToken+"?signed=1")
	signedViewURL := joinPublicURL(appCfg.PublicBaseURL, "/view/"+escapedToken+"?signed=1")

	return mailer.SendRequest{
		Template:    mailer.TemplateSignedDocument,
		Recipient:   recipient,
		MessageID:   token + ":signed",
		Correlation: token,
		Variables: map[string]string{
			"signed_download_url": signedDownloadURL,
			"signed_view_url":     signedViewURL,
		},
	}
}

func joinPublicURL(baseURL, path string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://signer.local"
	}
	if strings.HasPrefix(path, "/") {
		return baseURL + path
	}
	return baseURL + "/" + path
}

func notifyMailer(ctx context.Context, payload mailer.SendRequest) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, appCfg.MailerURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mailer returned %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}

	return nil
}

func handleSignRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req SignRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		var apiErr apiError
		if errors.As(err, &apiErr) {
			writeJSON(w, apiErr.Status, map[string]string{"error": apiErr.Message})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Bad Request"})
		return
	}

	signedURL, recipient, statusCode, err := signDocument(r.Context(), req)
	if err != nil {
		writeJSON(w, statusCode, map[string]string{"error": err.Error()})
		return
	}

	notifyCtx, cancel := context.WithTimeout(context.Background(), appCfg.DependencyTimeout)
	if err := notifyMailer(notifyCtx, buildSignedDocumentNotification(recipient, req.Token)); err != nil {
		log.Printf("Signed document notification failed for token=%s recipient=%s: %v", logutil.MaskToken(req.Token), logutil.MaskEmail(recipient), err)
	}
	cancel()

	writeJSON(w, http.StatusOK, map[string]string{
		"status":     "success",
		"signed_url": signedURL,
	})
	log.Printf("Document signed successfully: token=%s", logutil.MaskToken(req.Token))
}

func signDocument(ctx context.Context, req SignRequest) (string, string, int, error) {
	if strings.TrimSpace(req.Token) == "" || strings.TrimSpace(req.Password) == "" {
		return "", "", http.StatusBadRequest, apiError{Status: http.StatusBadRequest, Message: "token and password are required"}
	}

	var signedKey string
	signedStored := false
	signedURL := ""
	recipient := ""

	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session SigningSession
		result := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&session, "token = ?", req.Token)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return apiError{Status: http.StatusNotFound, Message: "Session not found"}
		}
		if result.Error != nil {
			return apiError{Status: http.StatusInternalServerError, Message: "Internal error"}
		}

		if session.Attempts >= MaxAttempts {
			return apiError{Status: http.StatusForbidden, Message: "Too many attempts. Session blocked."}
		}
		if session.IsUsed {
			return apiError{Status: http.StatusForbidden, Message: "Document already signed"}
		}

		if err := bcrypt.CompareHashAndPassword([]byte(session.CodeHash), []byte(req.Password)); err != nil {
			session.Attempts++
			if saveErr := tx.Save(&session).Error; saveErr != nil {
				return apiError{Status: http.StatusInternalServerError, Message: "Internal error"}
			}
			msg := fmt.Sprintf("Invalid code. Attempts remaining: %d", max(0, MaxAttempts-session.Attempts))
			return apiError{Status: http.StatusUnauthorized, Message: msg}
		}

		privKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return apiError{Status: http.StatusInternalServerError, Message: "Key gen failed"}
		}

		certPEM, keyPEM, err := generateSelfSignedCertPEM(session.Email, privKey)
		if err != nil {
			return apiError{Status: http.StatusInternalServerError, Message: "Cert gen failed"}
		}

		encryptedPrivKey, err := encryptAES(masterKey, keyPEM)
		if err != nil {
			return apiError{Status: http.StatusInternalServerError, Message: "Encryption failed"}
		}

		pdfBytes, err := getObjectBytes(ctx, s3Client, appCfg.MinioBucket, session.S3Key)
		if err != nil {
			return apiError{Status: http.StatusInternalServerError, Message: "Failed to load original PDF"}
		}

		signedPDF, err := signPDFViaService(ctx, appCfg.PDFSignURL, pdfBytes, certPEM, keyPEM, session.Token)
		if err != nil {
			log.Printf("pdfsigner error: %v", err)
			return apiError{Status: http.StatusInternalServerError, Message: "PDF signing failed"}
		}

		signedKey = "signed/" + session.S3Key
		if err := putObjectBytes(ctx, s3Client, appCfg.MinioBucket, signedKey, signedPDF, "application/pdf"); err != nil {
			return apiError{Status: http.StatusInternalServerError, Message: "Failed to store signed PDF"}
		}
		signedStored = true

		now := time.Now().UTC()
		session.IsUsed = true
		session.EncryptedPrivKey = encryptedPrivKey
		session.CertPEM = string(certPEM)
		session.SignedS3Key = signedKey
		session.SignedAt = &now
		if err := tx.Save(&session).Error; err != nil {
			return err
		}

		signedDoc := SignedDocument{
			Token:         session.Token,
			SignedS3Key:   signedKey,
			SignedPDFSHA:  sha256Hex(signedPDF),
			CertSHA:       certificatePEMSHA256(string(certPEM)),
			SignerSubject: extractCertificateSubject(certPEM, session.Email),
			SignedAt:      now,
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "signed_s3_key"}},
			DoUpdates: clause.Assignments(map[string]interface{}{
				"token":          signedDoc.Token,
				"signed_pdfsha":  signedDoc.SignedPDFSHA,
				"cert_sha":       signedDoc.CertSHA,
				"signer_subject": signedDoc.SignerSubject,
				"signed_at":      signedDoc.SignedAt,
			}),
		}).Create(&signedDoc).Error; err != nil {
			return err
		}

		signedURL = fmt.Sprintf("/download/%s?signed=1", session.Token)
		recipient = session.Email
		return nil
	})

	if err != nil {
		if signedStored && signedKey != "" {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), appCfg.DependencyTimeout)
			if deleteErr := infra.DeleteObject(cleanupCtx, s3Client, appCfg.MinioBucket, signedKey); deleteErr != nil {
				log.Printf("Signed PDF compensation delete failed for %s: %v", signedKey, deleteErr)
			}
			cancel()
		}

		var apiErr apiError
		if errors.As(err, &apiErr) {
			return "", "", apiErr.Status, apiErr
		}

		log.Printf("Signing transaction failed for token=%s: %v", logutil.MaskToken(req.Token), err)
		return "", "", http.StatusInternalServerError, apiError{Status: http.StatusInternalServerError, Message: "Internal error"}
	}

	return signedURL, recipient, http.StatusOK, nil
}

func handleVerifyRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req VerifyRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		var apiErr apiError
		if errors.As(err, &apiErr) {
			writeVerificationJSON(w, apiErr.Status, verificationError("error", apiErr.Message))
			return
		}
		writeVerificationJSON(w, http.StatusBadRequest, verificationError("error", "bad json"))
		return
	}

	switch {
	case strings.TrimSpace(req.Token) != "":
		handleVerifyByToken(w, r, req.Token)
	case strings.TrimSpace(req.UploadToken) != "":
		handleVerifyByUploadToken(w, r, req.UploadToken)
	default:
		writeVerificationJSON(w, http.StatusBadRequest, verificationError("error", "token or upload_token is required"))
	}
}

func handleVerifyByToken(w http.ResponseWriter, r *http.Request, token string) {
	if _, err := redisDB.Get(r.Context(), "doc:"+token).Result(); err != nil {
		if errors.Is(err, redis.Nil) {
			writeVerificationJSON(w, http.StatusNotFound, verificationError("error", "token not found or expired"))
			return
		}
		writeVerificationJSON(w, http.StatusInternalServerError, verificationError("error", "token lookup failed"))
		return
	}

	var session SigningSession
	dbResult := db.WithContext(r.Context()).First(&session, "token = ?", token)
	if errors.Is(dbResult.Error, gorm.ErrRecordNotFound) || session.SignedS3Key == "" {
		writeVerificationJSON(w, http.StatusNotFound, verificationError("error", "signed document not found"))
		return
	}
	if dbResult.Error != nil {
		writeVerificationJSON(w, http.StatusInternalServerError, verificationError("error", "database lookup failed"))
		return
	}

	pdfBytes, err := getObjectBytes(r.Context(), s3Client, appCfg.MinioBucket, session.SignedS3Key)
	if err != nil {
		writeVerificationJSON(w, http.StatusInternalServerError, verificationError("error", "failed to load signed PDF"))
		return
	}

	log.Printf("verify by token: token=%s signedKey=%s pdfSha=%s", logutil.MaskToken(token), session.SignedS3Key, sha256Hex(pdfBytes))
	statusCode, verification, err := verifyStoredServicePDF(r.Context(), pdfBytes, token, session.SignedS3Key)
	if err != nil {
		log.Printf("pdf verification error: %v", err)
		writeVerificationJSON(w, http.StatusInternalServerError, verificationError("error", "verification service failed"))
		return
	}

	writeVerificationJSON(w, statusCode, verification)
}

func handleVerifyByUploadToken(w http.ResponseWriter, r *http.Request, uploadToken string) {
	waitCtx, cancel := context.WithTimeout(r.Context(), appCfg.DependencyTimeout)
	defer cancel()

	val, err := waitForVerifyUpload(waitCtx, uploadToken)
	if err != nil {
		writeVerificationJSON(w, http.StatusNotFound, verificationError("error", "upload token not found or expired"))
		return
	}

	var meta FileMeta
	if err := json.Unmarshal([]byte(val), &meta); err != nil {
		writeVerificationJSON(w, http.StatusInternalServerError, verificationError("error", "invalid upload metadata"))
		return
	}
	if strings.TrimSpace(meta.S3Key) == "" {
		writeVerificationJSON(w, http.StatusInternalServerError, verificationError("error", "invalid upload metadata"))
		return
	}

	defer cleanupVerifyUpload(uploadToken, meta.S3Key)

	pdfBytes, err := getObjectBytes(r.Context(), s3Client, appCfg.MinioBucket, meta.S3Key)
	if err != nil {
		writeVerificationJSON(w, http.StatusInternalServerError, verificationError("error", "failed to load uploaded PDF"))
		return
	}

	log.Printf("verify by upload token: uploadToken=%s objectKey=%s pdfSha=%s", logutil.MaskToken(uploadToken), meta.S3Key, sha256Hex(pdfBytes))
	statusCode, verification, err := verifyServiceOwnedPDF(r.Context(), pdfBytes)
	if err != nil {
		log.Printf("pdf verification error: %v", err)
		writeVerificationJSON(w, http.StatusInternalServerError, verificationError("error", "verification service failed"))
		return
	}

	writeVerificationJSON(w, statusCode, verification)
}

func waitForVerifyUpload(ctx context.Context, uploadToken string) (string, error) {
	const delay = 300 * time.Millisecond

	var lastErr error
	for {
		val, err := redisDB.Get(ctx, "verify:"+uploadToken).Result()
		if err == nil {
			return val, nil
		}
		lastErr = err
		if err != nil && !errors.Is(err, redis.Nil) {
			log.Printf("verify upload redis lookup failed for %s: %v", logutil.MaskToken(uploadToken), err)
		}

		select {
		case <-ctx.Done():
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			return "", lastErr
		case <-time.After(delay):
		}
	}
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

func signPDFViaService(ctx context.Context, pdfSignURL string, pdfBytes, certPEM, keyPEM []byte, documentID string) ([]byte, error) {
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
	if err := w.WriteField("documentId", documentID); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pdfSignURL, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := httpClient.Do(req)
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

func verifyPDFViaService(ctx context.Context, pdfVerifyURL string, pdfBytes []byte) (int, []byte, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	pdfHdr := make(textproto.MIMEHeader)
	pdfHdr.Set("Content-Disposition", `form-data; name="pdf"; filename="document.pdf"`)
	pdfHdr.Set("Content-Type", "application/pdf")

	pdfPart, err := w.CreatePart(pdfHdr)
	if err != nil {
		return 0, nil, err
	}
	if _, err := pdfPart.Write(pdfBytes); err != nil {
		return 0, nil, err
	}
	if err := w.Close(); err != nil {
		return 0, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pdfVerifyURL, &buf)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}

func verifyServiceOwnedPDF(ctx context.Context, pdfBytes []byte) (int, VerificationResult, error) {
	documentHash := sha256Hex(pdfBytes)
	log.Printf("verify uploaded pdf: documentHash=%s", documentHash)

	var signedDoc SignedDocument
	result := db.WithContext(ctx).First(&signedDoc, "signed_pdfsha = ?", documentHash)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			log.Printf("verify uploaded pdf: no signed_documents match for hash=%s", documentHash)
			return verifyUnregisteredPDF(ctx, pdfBytes)
		}
		return 0, VerificationResult{}, result.Error
	}
	log.Printf("verify uploaded pdf: matched signed_documents record token=%s signedKey=%s hash=%s", logutil.MaskToken(signedDoc.Token), signedDoc.SignedS3Key, documentHash)

	return verifyViaPDFService(ctx, pdfBytes, true, "uploaded PDF matched signed_documents registry")
}

func verifyStoredServicePDF(ctx context.Context, pdfBytes []byte, token, signedS3Key string) (int, VerificationResult, error) {
	log.Printf("verify stored service pdf: token=%s signedKey=%s pdfSha=%s", logutil.MaskToken(token), signedS3Key, sha256Hex(pdfBytes))
	return verifyViaPDFService(ctx, pdfBytes, true, "stored service PDF verified via token lookup")
}

func verifyViaPDFService(ctx context.Context, pdfBytes []byte, serviceOwned bool, logPrefix string) (int, VerificationResult, error) {
	statusCode, respBody, err := verifyPDFViaService(ctx, derivePDFVerifyURL(appCfg.PDFSignURL), pdfBytes)
	if err != nil {
		return 0, VerificationResult{}, err
	}
	if statusCode == http.StatusBadRequest {
		return http.StatusInternalServerError, verificationError("error", "stored signed PDF could not be verified"), nil
	}
	if statusCode != http.StatusOK {
		return http.StatusInternalServerError, verificationError("error", "verification service returned an unexpected response"), nil
	}

	var verification VerificationResult
	if err := json.Unmarshal(respBody, &verification); err != nil {
		return 0, VerificationResult{}, err
	}
	verification.ServiceOwned = serviceOwned
	log.Printf(
		"%s: status=%s signaturePresent=%t integrityValid=%t certSha=%v",
		logPrefix,
		verification.Status,
		verification.SignaturePresent,
		verification.IntegrityValid,
		verification.CertificateSHA256,
	)
	return http.StatusOK, verification, nil
}

func verifyUnregisteredPDF(ctx context.Context, pdfBytes []byte) (int, VerificationResult, error) {
	statusCode, verification, err := verifyViaPDFService(ctx, pdfBytes, false, "verify uploaded pdf: unregistered artifact")
	if err != nil {
		return 0, VerificationResult{}, err
	}

	if verification.Status == "verified" {
		msg := "document is not signed by this service"
		verification.Status = "unknown_document"
		verification.ServiceOwned = false
		verification.Error = &msg
	}

	return statusCode, verification, nil
}

func derivePDFVerifyURL(pdfSignURL string) string {
	u, err := url.Parse(pdfSignURL)
	if err != nil {
		return strings.TrimRight(pdfSignURL, "/") + "/verify"
	}

	if strings.HasSuffix(u.Path, "/sign") {
		u.Path = strings.TrimSuffix(u.Path, "/sign") + "/verify"
	} else {
		u.Path = strings.TrimRight(u.Path, "/") + "/verify"
	}
	return u.String()
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

func verificationError(status, message string) VerificationResult {
	return VerificationResult{
		Status:           status,
		ServiceOwned:     false,
		SignaturePresent: false,
		IntegrityValid:   false,
		Error:            &message,
	}
}

func writeVerificationJSON(w http.ResponseWriter, statusCode int, result VerificationResult) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(result)
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if !strings.HasPrefix(contentType, "application/json") {
		return apiError{Status: http.StatusBadRequest, Message: "unsupported content type"}
	}

	r.Body = http.MaxBytesReader(w, r.Body, appCfg.JSONMaxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apiError{Status: http.StatusBadRequest, Message: "bad json"}
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return apiError{Status: http.StatusBadRequest, Message: "bad json"}
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func extractCertificateSubject(certPEM []byte, fallback string) string {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return fallback
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fallback
	}
	return cert.Subject.String()
}

func certificatePEMSHA256(certPEM string) string {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return ""
	}
	return sha256Hex(block.Bytes)
}

func cleanupVerifyUpload(uploadToken, objectKey string) {
	ctx, cancel := context.WithTimeout(context.Background(), appCfg.DependencyTimeout)
	defer cancel()

	if err := deleteVerifyArtifacts(ctx, s3Client, appCfg.MinioBucket, objectKey); err != nil {
		log.Printf("Verify upload delete failed for %s: %v", objectKey, err)
	}
	if err := redisDB.Del(ctx, "verify:"+uploadToken).Err(); err != nil {
		log.Printf("Verify upload redis delete failed for %s: %v", logutil.MaskToken(uploadToken), err)
	}
	if err := redisDB.ZRem(ctx, verifyCleanupZSetKey, objectKey).Err(); err != nil {
		log.Printf("Verify upload cleanup zset remove failed for %s: %v", objectKey, err)
	}
}

func deleteVerifyArtifacts(ctx context.Context, s3c *s3.Client, bucket, objectKey string) error {
	if err := infra.DeleteObject(ctx, s3c, bucket, objectKey); err != nil {
		return err
	}

	infoKey := objectKey + ".info"
	if err := infra.DeleteObject(ctx, s3c, bucket, infoKey); err != nil {
		log.Printf("Verify upload .info delete skipped for %s: %v", infoKey, err)
	}

	return nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
