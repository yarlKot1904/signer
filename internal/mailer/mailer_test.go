package mailer

import (
	"net/mail"
	"strings"
	"testing"
)

func TestRenderSigningOTP(t *testing.T) {
	msg, err := Render(SendRequest{
		Template:  TemplateSigningOTP,
		Recipient: "user@example.com",
		Variables: map[string]string{
			"code":         "123456",
			"sign_url":     "http://localhost/sign.html?token=abc",
			"download_url": "http://localhost/download/abc",
			"view_url":     "http://localhost/view/abc",
		},
	})
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if msg.Subject != "Signer OTP code" {
		t.Fatalf("unexpected subject: %s", msg.Subject)
	}
	if msg.Metadata["has_sign_url"] != "true" {
		t.Fatalf("expected has_sign_url metadata to be true")
	}
	if msg.Body == "" {
		t.Fatal("expected message body to be rendered")
	}
}

func TestRenderSigningOTPMissingVariable(t *testing.T) {
	_, err := Render(SendRequest{
		Template:  TemplateSigningOTP,
		Recipient: "user@example.com",
		Variables: map[string]string{
			"code": "123456",
		},
	})
	if err == nil {
		t.Fatal("expected missing variable error")
	}
}

func TestRenderSignedDocument(t *testing.T) {
	msg, err := Render(SendRequest{
		Template:  TemplateSignedDocument,
		Recipient: "user@example.com",
		Variables: map[string]string{
			"signed_download_url": "http://localhost/download/abc?signed=1",
			"signed_view_url":     "http://localhost/view/abc?signed=1",
		},
	})
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if msg.Subject != "Signer signed document" {
		t.Fatalf("unexpected subject: %s", msg.Subject)
	}
	if msg.Metadata["has_signed_download_url"] != "true" {
		t.Fatalf("expected has_signed_download_url metadata to be true")
	}
	if msg.Body == "" {
		t.Fatal("expected message body to be rendered")
	}
}

func TestRenderSignedDocumentMissingVariable(t *testing.T) {
	_, err := Render(SendRequest{
		Template:  TemplateSignedDocument,
		Recipient: "user@example.com",
		Variables: map[string]string{
			"signed_download_url": "http://localhost/download/abc?signed=1",
		},
	})
	if err == nil {
		t.Fatal("expected missing variable error")
	}
}

func TestNewSMTPSenderValidatesRequiredConfig(t *testing.T) {
	_, err := NewSMTPSender(SMTPConfig{
		Host: "smtp.example.com",
		From: "Signer <no-reply@example.com>",
	})
	if err != nil {
		t.Fatalf("expected SMTP config to be valid: %v", err)
	}

	_, err = NewSMTPSender(SMTPConfig{
		Host: "smtp.example.com",
	})
	if err == nil {
		t.Fatal("expected SMTP_FROM to be required")
	}
}

func TestBuildSMTPMessage(t *testing.T) {
	from, err := mail.ParseAddress("Signer <no-reply@example.com>")
	if err != nil {
		t.Fatalf("parse from address: %v", err)
	}
	to, err := mail.ParseAddress("user@example.com")
	if err != nil {
		t.Fatalf("parse to address: %v", err)
	}

	raw, err := buildSMTPMessage(from, to, Message{
		Template:    TemplateSignedDocument,
		Recipient:   "user@example.com",
		Subject:     "Signed document",
		Body:        "Your signed PDF is ready.",
		MessageID:   "msg-123",
		Correlation: "token-123",
	})
	if err != nil {
		t.Fatalf("build SMTP message: %v", err)
	}

	message := string(raw)
	for _, want := range []string{
		`From: "Signer" <no-reply@example.com>` + "\r\n",
		"To: <user@example.com>\r\n",
		"Subject: Signed document\r\n",
		"Content-Transfer-Encoding: quoted-printable\r\n",
		"X-Signer-Template: signed-document\r\n",
		"X-Signer-Message-Id: msg-123\r\n",
		"X-Correlation-Id: token-123\r\n",
		"\r\nYour signed PDF is ready.\r\n",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("expected SMTP message to contain %q, got:\n%s", want, message)
		}
	}
}

func TestBuildSMTPMessageRejectsHeaderInjection(t *testing.T) {
	from, err := mail.ParseAddress("Signer <no-reply@example.com>")
	if err != nil {
		t.Fatalf("parse from address: %v", err)
	}
	to, err := mail.ParseAddress("user@example.com")
	if err != nil {
		t.Fatalf("parse to address: %v", err)
	}

	_, err = buildSMTPMessage(from, to, Message{
		Template:  TemplateSigningOTP,
		Recipient: "user@example.com",
		Subject:   "OTP\r\nBcc: attacker@example.com",
		Body:      "Your code is 123456.",
	})
	if err == nil {
		t.Fatal("expected header injection to be rejected")
	}
}
