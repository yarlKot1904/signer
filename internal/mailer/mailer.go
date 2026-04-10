package mailer

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/yarlKot1904/signer/internal/logutil"
)

const (
	TemplateSigningOTP     = "signing-otp"
	TemplateSignedDocument = "signed-document"
)

type SendRequest struct {
	Template    string            `json:"template"`
	Recipient   string            `json:"recipient"`
	Subject     string            `json:"subject,omitempty"`
	Variables   map[string]string `json:"variables"`
	MessageID   string            `json:"message_id,omitempty"`
	Correlation string            `json:"correlation,omitempty"`
}

type Message struct {
	Template    string
	Recipient   string
	Subject     string
	Body        string
	MessageID   string
	Correlation string
	Metadata    map[string]string
}

type Sender interface {
	Send(context.Context, Message) error
}

type LogSender struct {
	IncludeBody bool
}

func (s LogSender) Send(_ context.Context, msg Message) error {
	metadata, err := json.Marshal(msg.Metadata)
	if err != nil {
		return fmt.Errorf("marshal mail metadata: %w", err)
	}

	if s.IncludeBody {
		log.Printf(
			"Mailer dispatch: transport=log template=%s recipient=%s messageID=%s correlation=%s subject=%q body=%q metadata=%s",
			msg.Template,
			logutil.MaskEmail(msg.Recipient),
			msg.MessageID,
			msg.Correlation,
			msg.Subject,
			msg.Body,
			string(metadata),
		)
		return nil
	}

	log.Printf(
		"Mailer dispatch: transport=log template=%s recipient=%s messageID=%s correlation=%s subject=%q bodyLogged=false metadata=%s",
		msg.Template,
		logutil.MaskEmail(msg.Recipient),
		msg.MessageID,
		msg.Correlation,
		msg.Subject,
		string(metadata),
	)
	return nil
}

func Render(req SendRequest) (Message, error) {
	switch req.Template {
	case TemplateSigningOTP:
		return renderSigningOTP(req)
	case TemplateSignedDocument:
		return renderSignedDocument(req)
	default:
		return Message{}, fmt.Errorf("unsupported template: %s", req.Template)
	}
}

func renderSigningOTP(req SendRequest) (Message, error) {
	requiredKeys := []string{"code", "sign_url", "download_url", "view_url"}
	for _, key := range requiredKeys {
		if req.Variables[key] == "" {
			return Message{}, fmt.Errorf("missing variable %q for template %s", key, req.Template)
		}
	}

	subject := req.Subject
	if subject == "" {
		subject = "Signer OTP code"
	}

	body := fmt.Sprintf(
		"Your one-time password for PDF signing is %s.\n\nSign document: %s\nDownload original PDF: %s\nPreview document: %s\n",
		req.Variables["code"],
		req.Variables["sign_url"],
		req.Variables["download_url"],
		req.Variables["view_url"],
	)

	metadata := map[string]string{
		"code_length":      "6",
		"has_sign_url":     fmt.Sprintf("%t", req.Variables["sign_url"] != ""),
		"has_view_url":     fmt.Sprintf("%t", req.Variables["view_url"] != ""),
		"has_download_url": fmt.Sprintf("%t", req.Variables["download_url"] != ""),
	}

	return Message{
		Template:    req.Template,
		Recipient:   req.Recipient,
		Subject:     subject,
		Body:        body,
		MessageID:   req.MessageID,
		Correlation: req.Correlation,
		Metadata:    metadata,
	}, nil
}

func renderSignedDocument(req SendRequest) (Message, error) {
	requiredKeys := []string{"signed_download_url", "signed_view_url"}
	for _, key := range requiredKeys {
		if req.Variables[key] == "" {
			return Message{}, fmt.Errorf("missing variable %q for template %s", key, req.Template)
		}
	}

	subject := req.Subject
	if subject == "" {
		subject = "Signer signed document"
	}

	body := fmt.Sprintf(
		"Your signed PDF is ready.\n\nDownload signed PDF: %s\nPreview signed PDF: %s\n",
		req.Variables["signed_download_url"],
		req.Variables["signed_view_url"],
	)

	metadata := map[string]string{
		"has_signed_download_url": fmt.Sprintf("%t", req.Variables["signed_download_url"] != ""),
		"has_signed_view_url":     fmt.Sprintf("%t", req.Variables["signed_view_url"] != ""),
	}

	return Message{
		Template:    req.Template,
		Recipient:   req.Recipient,
		Subject:     subject,
		Body:        body,
		MessageID:   req.MessageID,
		Correlation: req.Correlation,
		Metadata:    metadata,
	}, nil
}
