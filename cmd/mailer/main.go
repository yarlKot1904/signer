package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yarlKot1904/signer/internal/config"
	"github.com/yarlKot1904/signer/internal/mailer"
	appmetrics "github.com/yarlKot1904/signer/internal/metrics"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("Config error:", err)
	}

	appCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	appmetrics.StartServer(appCtx, cfg.MetricsPort, "Mailer", cfg.ShutdownTimeout)

	sender, transport, err := buildSender(cfg)
	if err != nil {
		log.Fatal("Mailer transport config error:", err)
	}
	if cfg.MailerLogBody {
		appmetrics.MailerLogBodyEnabled.Set(1)
	}
	tlsMode := strings.ToLower(strings.TrimSpace(cfg.SMTPTLSMode))
	if transport == "log" || tlsMode == "" {
		tlsMode = "none"
	}
	appmetrics.MailerTransportInfo.WithLabelValues(transport, tlsMode).Set(1)

	mux := http.NewServeMux()
	mux.HandleFunc("/send", appmetrics.InstrumentHandlerFunc("mailer", "/send", func(w http.ResponseWriter, r *http.Request) {
		handleSendRequest(w, r, sender, transport)
	}))
	mux.HandleFunc("/health", appmetrics.InstrumentHandlerFunc("mailer", "/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))

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
			log.Printf("Mailer shutdown failed: %v", err)
		}
	}()

	log.Printf("Mailer service started on :%s transport=%s bodyLogged=%t", cfg.HTTPPort, transport, cfg.MailerLogBody)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func buildSender(cfg *config.Config) (mailer.Sender, string, error) {
	transport := strings.ToLower(strings.TrimSpace(cfg.MailerTransport))
	if transport == "" {
		transport = "log"
	}

	switch transport {
	case "log":
		return mailer.LogSender{IncludeBody: cfg.MailerLogBody}, transport, nil
	case "smtp":
		sender, err := mailer.NewSMTPSender(mailer.SMTPConfig{
			Host:       cfg.SMTPHost,
			Port:       cfg.SMTPPort,
			Username:   cfg.SMTPUsername,
			Password:   cfg.SMTPPassword,
			From:       cfg.SMTPFrom,
			TLSMode:    cfg.SMTPTLSMode,
			ServerName: cfg.SMTPServerName,
			Timeout:    cfg.DependencyTimeout,
		})
		return sender, transport, err
	default:
		return nil, "", errors.New("MAILER_TRANSPORT must be log or smtp")
	}
}

type apiError struct {
	Status  int
	Message string
}

func (e apiError) Error() string {
	return e.Message
}

func handleSendRequest(w http.ResponseWriter, r *http.Request, sender mailer.Sender, transport string) {
	start := time.Now()
	template := "unknown"
	result := "error"
	defer func() {
		appmetrics.MailerSendRequests.WithLabelValues(template, transport, result).Inc()
		appmetrics.MailerSendDuration.WithLabelValues(template, transport, result).Observe(time.Since(start).Seconds())
	}()

	if r.Method != http.MethodPost {
		result = "bad_request"
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req mailer.SendRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		result = "bad_request"
		var apiErr apiError
		if errors.As(err, &apiErr) {
			writeJSON(w, apiErr.Status, map[string]string{"error": apiErr.Message})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	template = boundedTemplate(req.Template)

	renderStart := time.Now()
	msg, err := mailer.Render(req)
	renderResult := appmetrics.ResultFromErr(err)
	appmetrics.MailerRenderDuration.WithLabelValues(template, renderResult).Observe(time.Since(renderStart).Seconds())
	if err != nil {
		result = "bad_request"
		appmetrics.MailerRenderFailures.WithLabelValues(template).Inc()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if transport != "smtp" {
		appmetrics.MailerMessageBytes.WithLabelValues(template, transport).Observe(float64(len(msg.Body)))
	}

	if err := sender.Send(r.Context(), msg); err != nil {
		log.Printf("Mailer send failed: template=%s messageID=%s correlation=%s err=%v", req.Template, req.MessageID, req.Correlation, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "send failed"})
		return
	}

	result = "success"
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func boundedTemplate(template string) string {
	switch template {
	case mailer.TemplateSigningOTP, mailer.TemplateSignedDocument:
		return template
	default:
		return "unknown"
	}
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
