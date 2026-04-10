package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yarlKot1904/signer/internal/config"
	"github.com/yarlKot1904/signer/internal/mailer"
)

func TestHandleSignRequestSendsSignedDocumentNotification(t *testing.T) {
	previousCfg := appCfg
	previousSignDocumentFunc := signDocumentFunc
	previousNotifyMailerFunc := notifyMailerFunc
	defer func() {
		appCfg = previousCfg
		signDocumentFunc = previousSignDocumentFunc
		notifyMailerFunc = previousNotifyMailerFunc
	}()

	appCfg = &config.Config{
		PublicBaseURL:     "http://localhost",
		DependencyTimeout: time.Second,
		JSONMaxBytes:      1024,
	}

	signDocumentFunc = func(_ context.Context, req SignRequest) (string, string, int, error) {
		if req.Token != "abc-token" {
			t.Fatalf("unexpected token: %s", req.Token)
		}
		if req.Password != "123456" {
			t.Fatalf("unexpected password: %s", req.Password)
		}
		return "/download/abc-token?signed=1", "user@example.com", http.StatusOK, nil
	}

	var gotNotification mailer.SendRequest
	notifyMailerFunc = func(_ context.Context, payload mailer.SendRequest) error {
		gotNotification = payload
		return nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sign", strings.NewReader(`{"token":"abc-token","password":"123456"}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	handleSignRequest(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", recorder.Code)
	}

	var response map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["signed_url"] != "/download/abc-token?signed=1" {
		t.Fatalf("unexpected signed_url: %s", response["signed_url"])
	}

	if gotNotification.Template != mailer.TemplateSignedDocument {
		t.Fatalf("unexpected template: %s", gotNotification.Template)
	}
	if gotNotification.Recipient != "user@example.com" {
		t.Fatalf("unexpected recipient: %s", gotNotification.Recipient)
	}
	if gotNotification.Variables["signed_download_url"] != "http://localhost/download/abc-token?signed=1" {
		t.Fatalf("unexpected signed download url: %s", gotNotification.Variables["signed_download_url"])
	}
	if gotNotification.Variables["signed_view_url"] != "http://localhost/view/abc-token?signed=1" {
		t.Fatalf("unexpected signed view url: %s", gotNotification.Variables["signed_view_url"])
	}
}
