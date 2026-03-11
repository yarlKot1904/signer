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
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"github.com/yarlKot1904/signer/internal/config"
	"github.com/yarlKot1904/signer/internal/infra"
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

const verifyCleanupZSetKey = "verify:cleanup"

var (
	db        *gorm.DB
	appCfg    *config.Config
	appCtx    context.Context
	s3Client  *s3.Client
	redisDB   *redis.Client
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

	redisDB, err = infra.NewRedisClient(appCfg.RedisAddr)
	if err != nil {
		log.Fatal("Redis connect failed:", err)
	}

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
	http.HandleFunc("/api/verify", handleVerifyRequest)

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
	if err := db.Save(&session).Error; err != nil {
		log.Printf("DB save session error: %v", err)
		http.Error(w, `{"error":"Failed to persist signing session"}`, http.StatusInternalServerError)
		return
	}

	signedDoc := SignedDocument{
		Token:         session.Token,
		SignedS3Key:   signedKey,
		SignedPDFSHA:  sha256Hex(signedPdf),
		CertSHA:       sha256Hex(certPEM),
		SignerSubject: extractCertificateSubject(certPEM, session.Email),
		SignedAt:      now,
	}
	if err := db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "signed_s3_key"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"token":          signedDoc.Token,
			"signed_pdfsha":  signedDoc.SignedPDFSHA,
			"cert_sha":       signedDoc.CertSHA,
			"signer_subject": signedDoc.SignerSubject,
			"signed_at":      signedDoc.SignedAt,
		}),
	}).Create(&signedDoc).Error; err != nil {
		log.Printf("DB save signed document error: %v", err)
		http.Error(w, `{"error":"Failed to persist signed document metadata"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(fmt.Sprintf(`{"status":"success","signed_url":"/download/%s?signed=1"}`, session.Token)))
	log.Printf("SIGNED URL: http://signer.local/download/%s?signed=1", session.Token)
}

func handleVerifyRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if !strings.HasPrefix(contentType, "application/json") {
		writeVerificationJSON(w, http.StatusBadRequest, verificationError("error", "unsupported content type"))
		return
	}
	handleVerifyByToken(w, r)
}

func handleVerifyByToken(w http.ResponseWriter, r *http.Request) {
	var req VerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeVerificationJSON(w, http.StatusBadRequest, verificationError("error", "bad json"))
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		if strings.TrimSpace(req.UploadToken) == "" {
			writeVerificationJSON(w, http.StatusBadRequest, verificationError("error", "token or upload_token is required"))
			return
		}
		handleVerifyByUploadToken(w, req.UploadToken)
		return
	}

	if _, err := redisDB.Get(appCtx, "doc:"+req.Token).Result(); err != nil {
		writeVerificationJSON(w, http.StatusNotFound, verificationError("error", "token not found or expired"))
		return
	}

	var session SigningSession
	dbResult := db.First(&session, "token = ?", req.Token)
	if errors.Is(dbResult.Error, gorm.ErrRecordNotFound) || session.SignedS3Key == "" {
		writeVerificationJSON(w, http.StatusNotFound, verificationError("error", "signed document not found"))
		return
	} else if dbResult.Error != nil {
		writeVerificationJSON(w, http.StatusInternalServerError, verificationError("error", "database lookup failed"))
		return
	}

	pdfBytes, err := getObjectBytes(appCtx, s3Client, appCfg.MinioBucket, session.SignedS3Key)
	if err != nil {
		writeVerificationJSON(w, http.StatusInternalServerError, verificationError("error", "failed to load signed PDF"))
		return
	}

	statusCode, verification, err := verifyServiceOwnedPDF(pdfBytes)
	if err != nil {
		log.Printf("pdf verification error: %v", err)
		writeVerificationJSON(w, http.StatusInternalServerError, verificationError("error", "verification service failed"))
		return
	}

	writeVerificationJSON(w, statusCode, verification)
}

func handleVerifyByUploadToken(w http.ResponseWriter, uploadToken string) {
	val, err := redisDB.Get(appCtx, "verify:"+uploadToken).Result()
	if err != nil {
		writeVerificationJSON(w, http.StatusNotFound, verificationError("error", "upload token not found or expired"))
		return
	}

	var meta FileMeta
	if err := json.Unmarshal([]byte(val), &meta); err != nil {
		writeVerificationJSON(w, http.StatusInternalServerError, verificationError("error", "invalid upload metadata"))
		return
	}

	pdfBytes, err := getObjectBytes(appCtx, s3Client, appCfg.MinioBucket, meta.S3Key)
	if err != nil {
		writeVerificationJSON(w, http.StatusInternalServerError, verificationError("error", "failed to load uploaded PDF"))
		return
	}

	statusCode, verification, err := verifyServiceOwnedPDF(pdfBytes)
	if err != nil {
		log.Printf("pdf verification error: %v", err)
		writeVerificationJSON(w, http.StatusInternalServerError, verificationError("error", "verification service failed"))
		return
	}
	defer cleanupVerifyUpload(uploadToken, meta.S3Key)
	writeVerificationJSON(w, statusCode, verification)
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

func verifyPDFViaService(pdfVerifyURL string, pdfBytes []byte) (int, []byte, error) {
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

	req, err := http.NewRequest(http.MethodPost, pdfVerifyURL, &buf)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
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

func verifyServiceOwnedPDF(pdfBytes []byte) (int, VerificationResult, error) {
	documentHash := sha256Hex(pdfBytes)

	var signedDoc SignedDocument
	result := db.First(&signedDoc, "signed_pdfsha = ?", documentHash)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return verifyServiceOwnedByCertificate(pdfBytes)
		}
		return 0, VerificationResult{}, result.Error
	}

	statusCode, respBody, err := verifyPDFViaService(derivePDFVerifyURL(appCfg.PDFSignURL), pdfBytes)
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
	verification.ServiceOwned = true
	return http.StatusOK, verification, nil
}

func verifyServiceOwnedByCertificate(pdfBytes []byte) (int, VerificationResult, error) {
	statusCode, respBody, err := verifyPDFViaService(derivePDFVerifyURL(appCfg.PDFSignURL), pdfBytes)
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

	if verification.CertificateSHA256 != nil {
		var sessions []SigningSession
		if err := db.Where("cert_pem IS NOT NULL AND cert_pem <> ''").Find(&sessions).Error; err != nil {
			return 0, VerificationResult{}, err
		}

		for _, session := range sessions {
			if sha256Hex([]byte(session.CertPEM)) == *verification.CertificateSHA256 {
				verification.ServiceOwned = true
				return http.StatusOK, verification, nil
			}
		}
	}

	msg := "document is not signed by this service"
	return http.StatusOK, VerificationResult{
		Status:                "unknown_document",
		ServiceOwned:          false,
		SignaturePresent:      verification.SignaturePresent,
		IntegrityValid:        verification.IntegrityValid,
		SignerSubject:         verification.SignerSubject,
		SignerCN:              verification.SignerCN,
		SigningTime:           verification.SigningTime,
		CertificateSelfSigned: verification.CertificateSelfSigned,
		CertificateSHA256:     verification.CertificateSHA256,
		CertificateTrusted:    verification.CertificateTrusted,
		Error:                 &msg,
	}, nil
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

func cleanupVerifyUpload(uploadToken, objectKey string) {
	if err := infra.DeleteObject(appCtx, s3Client, appCfg.MinioBucket, objectKey); err != nil {
		log.Printf("Verify upload delete failed for %s: %v", objectKey, err)
	}
	if err := redisDB.Del(appCtx, "verify:"+uploadToken).Err(); err != nil {
		log.Printf("Verify upload redis delete failed for %s: %v", uploadToken, err)
	}
	if err := redisDB.ZRem(appCtx, verifyCleanupZSetKey, objectKey).Err(); err != nil {
		log.Printf("Verify upload cleanup zset remove failed for %s: %v", objectKey, err)
	}
}
