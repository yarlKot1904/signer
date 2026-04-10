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

	"github.com/yarlKot1904/signer/internal/config"
	"github.com/yarlKot1904/signer/internal/mailer"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("Config error:", err)
	}

	appCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sender := mailer.LogSender{IncludeBody: cfg.MailerLogBody}

	mux := http.NewServeMux()
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		handleSendRequest(w, r, sender)
	})
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
			log.Printf("Mailer shutdown failed: %v", err)
		}
	}()

	log.Printf("Mailer service started on :%s transport=log bodyLogged=%t", cfg.HTTPPort, cfg.MailerLogBody)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

type apiError struct {
	Status  int
	Message string
}

func (e apiError) Error() string {
	return e.Message
}

func handleSendRequest(w http.ResponseWriter, r *http.Request, sender mailer.Sender) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req mailer.SendRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		var apiErr apiError
		if errors.As(err, &apiErr) {
			writeJSON(w, apiErr.Status, map[string]string{"error": apiErr.Message})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}

	msg, err := mailer.Render(req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if err := sender.Send(r.Context(), msg); err != nil {
		log.Printf("Mailer send failed: template=%s messageID=%s correlation=%s err=%v", req.Template, req.MessageID, req.Correlation, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "send failed"})
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
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
