package mailer

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"github.com/yarlKot1904/signer/internal/logutil"
	appmetrics "github.com/yarlKot1904/signer/internal/metrics"
)

const (
	SMTPTLSModeStartTLS = "starttls"
	SMTPTLSModeImplicit = "implicit"
	SMTPTLSModeNone     = "none"
)

type SMTPConfig struct {
	Host       string
	Port       string
	Username   string
	Password   string
	From       string
	TLSMode    string
	ServerName string
	Timeout    time.Duration
}

type SMTPSender struct {
	cfg SMTPConfig
}

func NewSMTPSender(cfg SMTPConfig) (*SMTPSender, error) {
	cfg = normalizeSMTPConfig(cfg)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &SMTPSender{cfg: cfg}, nil
}

func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	ctx, cancel := s.contextWithTimeout(ctx)
	defer cancel()

	from, err := mail.ParseAddress(s.cfg.From)
	if err != nil {
		appmetrics.MailerInvalidRecipient.WithLabelValues(boundedTemplate(msg.Template)).Inc()
		return fmt.Errorf("parse SMTP from address: %w", err)
	}
	to, err := mail.ParseAddress(msg.Recipient)
	if err != nil {
		appmetrics.MailerInvalidRecipient.WithLabelValues(boundedTemplate(msg.Template)).Inc()
		return fmt.Errorf("parse SMTP recipient address: %w", err)
	}

	rawMessage, err := buildSMTPMessage(from, to, msg)
	if err != nil {
		return err
	}
	appmetrics.MailerMessageBytes.WithLabelValues(boundedTemplate(msg.Template), "smtp").Observe(float64(len(rawMessage)))

	connectStart := time.Now()
	client, err := s.newClient(ctx)
	appmetrics.MailerSMTPConnectDuration.WithLabelValues(s.cfg.TLSMode, appmetrics.ResultFromErr(err)).Observe(time.Since(connectStart).Seconds())
	if err != nil {
		return err
	}
	defer client.Close()

	if s.cfg.Username != "" {
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.ServerName)
		stageStart := time.Now()
		err := client.Auth(auth)
		recordSMTPStage("auth", s.cfg.TLSMode, stageStart, err)
		appmetrics.MailerSMTPAuth.WithLabelValues(appmetrics.ResultFromErr(err)).Inc()
		if err != nil {
			return fmt.Errorf("SMTP auth: %w", err)
		}
	}
	stageStart := time.Now()
	if err := client.Mail(from.Address); err != nil {
		recordSMTPStage("mail_from", s.cfg.TLSMode, stageStart, err)
		return fmt.Errorf("SMTP MAIL FROM: %w", err)
	}
	recordSMTPStage("mail_from", s.cfg.TLSMode, stageStart, nil)

	stageStart = time.Now()
	if err := client.Rcpt(to.Address); err != nil {
		recordSMTPStage("rcpt_to", s.cfg.TLSMode, stageStart, err)
		return fmt.Errorf("SMTP RCPT TO: %w", err)
	}
	recordSMTPStage("rcpt_to", s.cfg.TLSMode, stageStart, nil)

	stageStart = time.Now()
	writer, err := client.Data()
	if err != nil {
		recordSMTPStage("data", s.cfg.TLSMode, stageStart, err)
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	if _, err := writer.Write(rawMessage); err != nil {
		_ = writer.Close()
		recordSMTPStage("data", s.cfg.TLSMode, stageStart, err)
		return fmt.Errorf("write SMTP message: %w", err)
	}
	if err := writer.Close(); err != nil {
		recordSMTPStage("data", s.cfg.TLSMode, stageStart, err)
		return fmt.Errorf("finish SMTP message: %w", err)
	}
	recordSMTPStage("data", s.cfg.TLSMode, stageStart, nil)

	stageStart = time.Now()
	if err := client.Quit(); err != nil {
		recordSMTPStage("quit", s.cfg.TLSMode, stageStart, err)
		return fmt.Errorf("SMTP quit: %w", err)
	}
	recordSMTPStage("quit", s.cfg.TLSMode, stageStart, nil)

	log.Printf(
		"Mailer dispatch: transport=smtp template=%s recipient=%s messageID=%s correlation=%s subject=%q host=%s tlsMode=%s",
		msg.Template,
		logutil.MaskEmail(msg.Recipient),
		msg.MessageID,
		msg.Correlation,
		msg.Subject,
		s.cfg.Host,
		s.cfg.TLSMode,
	)
	return nil
}

func (s *SMTPSender) contextWithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.cfg.Timeout <= 0 {
		return ctx, func() {}
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, s.cfg.Timeout)
}

func (s *SMTPSender) newClient(ctx context.Context) (*smtp.Client, error) {
	addr := net.JoinHostPort(s.cfg.Host, s.cfg.Port)
	dialer := &net.Dialer{}
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: s.cfg.ServerName,
	}

	var conn net.Conn
	var err error
	stageStart := time.Now()
	if s.cfg.TLSMode == SMTPTLSModeImplicit {
		tlsDialer := tls.Dialer{
			NetDialer: dialer,
			Config:    tlsConfig,
		}
		conn, err = tlsDialer.DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	recordSMTPStage("dial", s.cfg.TLSMode, stageStart, err)
	if err != nil {
		return nil, fmt.Errorf("dial SMTP %s: %w", addr, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	client, err := smtp.NewClient(conn, s.cfg.ServerName)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("create SMTP client: %w", err)
	}

	if s.cfg.TLSMode == SMTPTLSModeStartTLS {
		ok, _ := client.Extension("STARTTLS")
		if !ok {
			_ = client.Close()
			err := fmt.Errorf("SMTP server %s does not advertise STARTTLS", addr)
			recordSMTPStage("starttls", s.cfg.TLSMode, time.Now(), err)
			return nil, err
		}
		stageStart = time.Now()
		if err := client.StartTLS(tlsConfig); err != nil {
			_ = client.Close()
			recordSMTPStage("starttls", s.cfg.TLSMode, stageStart, err)
			return nil, fmt.Errorf("SMTP STARTTLS: %w", err)
		}
		recordSMTPStage("starttls", s.cfg.TLSMode, stageStart, nil)
	}
	return client, nil
}

func recordSMTPStage(stage, tlsMode string, start time.Time, err error) {
	result := appmetrics.ResultFromErr(err)
	appmetrics.MailerSMTPStageTotal.WithLabelValues(stage, tlsMode, result).Inc()
	appmetrics.MailerSMTPStageDuration.WithLabelValues(stage, tlsMode, result).Observe(time.Since(start).Seconds())
}

func boundedTemplate(template string) string {
	switch template {
	case TemplateSigningOTP, TemplateSignedDocument:
		return template
	default:
		return "unknown"
	}
}

func buildSMTPMessage(from, to *mail.Address, msg Message) ([]byte, error) {
	subject := msg.Subject
	if subject == "" {
		subject = "Signer notification"
	}
	if strings.ContainsAny(subject, "\r\n") {
		return nil, fmt.Errorf("SMTP subject contains a line break")
	}

	body := &bytes.Buffer{}
	qpWriter := quotedprintable.NewWriter(body)
	if _, err := io.WriteString(qpWriter, msg.Body); err != nil {
		return nil, fmt.Errorf("encode SMTP body: %w", err)
	}
	if err := qpWriter.Close(); err != nil {
		return nil, fmt.Errorf("finish SMTP body encoding: %w", err)
	}

	headers := []struct {
		key   string
		value string
	}{
		{"From", from.String()},
		{"To", to.String()},
		{"Subject", mime.QEncoding.Encode("UTF-8", subject)},
		{"Date", time.Now().UTC().Format(time.RFC1123Z)},
		{"MIME-Version", "1.0"},
		{"Content-Type", `text/plain; charset="utf-8"`},
		{"Content-Transfer-Encoding", "quoted-printable"},
		{"X-Signer-Template", msg.Template},
	}
	if msg.MessageID != "" {
		headers = append(headers, struct {
			key   string
			value string
		}{"X-Signer-Message-ID", msg.MessageID})
	}
	if msg.Correlation != "" {
		headers = append(headers, struct {
			key   string
			value string
		}{"X-Correlation-ID", msg.Correlation})
	}

	out := &bytes.Buffer{}
	for _, header := range headers {
		if err := writeHeader(out, header.key, header.value); err != nil {
			return nil, err
		}
	}
	out.WriteString("\r\n")
	out.Write(body.Bytes())
	out.WriteString("\r\n")
	return out.Bytes(), nil
}

func writeHeader(out *bytes.Buffer, key, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("SMTP header %s contains a line break", key)
	}
	_, _ = fmt.Fprintf(out, "%s: %s\r\n", textproto.CanonicalMIMEHeaderKey(key), value)
	return nil
}

func normalizeSMTPConfig(cfg SMTPConfig) SMTPConfig {
	cfg.Host = strings.TrimSpace(cfg.Host)
	cfg.Port = strings.TrimSpace(cfg.Port)
	if cfg.Port == "" {
		cfg.Port = "587"
	}
	cfg.Username = strings.TrimSpace(cfg.Username)
	cfg.From = strings.TrimSpace(cfg.From)
	cfg.TLSMode = strings.ToLower(strings.TrimSpace(cfg.TLSMode))
	if cfg.TLSMode == "" {
		cfg.TLSMode = SMTPTLSModeStartTLS
	}
	if cfg.TLSMode == "tls" || cfg.TLSMode == "ssl" {
		cfg.TLSMode = SMTPTLSModeImplicit
	}
	cfg.ServerName = strings.TrimSpace(cfg.ServerName)
	if cfg.ServerName == "" {
		cfg.ServerName = cfg.Host
	}
	return cfg
}

func (cfg SMTPConfig) validate() error {
	if cfg.Host == "" {
		return fmt.Errorf("SMTP_HOST is required when MAILER_TRANSPORT=smtp")
	}
	if cfg.From == "" {
		return fmt.Errorf("SMTP_FROM is required when MAILER_TRANSPORT=smtp")
	}
	if _, err := mail.ParseAddress(cfg.From); err != nil {
		return fmt.Errorf("parse SMTP_FROM: %w", err)
	}
	if cfg.Username == "" && cfg.Password != "" {
		return fmt.Errorf("SMTP_USERNAME is required when SMTP_PASSWORD is set")
	}
	if cfg.Username != "" && cfg.Password == "" {
		return fmt.Errorf("SMTP_PASSWORD is required when SMTP_USERNAME is set")
	}
	switch cfg.TLSMode {
	case SMTPTLSModeStartTLS, SMTPTLSModeImplicit, SMTPTLSModeNone:
		return nil
	default:
		return fmt.Errorf("SMTP_TLS_MODE must be one of %s, %s, or %s", SMTPTLSModeStartTLS, SMTPTLSModeImplicit, SMTPTLSModeNone)
	}
}
