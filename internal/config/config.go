package config

import (
	"time"

	"github.com/kelseyhightower/envconfig"
)

type Config struct {
	MinioEndpoint string `envconfig:"MINIO_ENDPOINT"`
	MinioID       string `envconfig:"MINIO_ID"`
	MinioSecret   string `envconfig:"MINIO_SECRET"`
	MinioBucket   string `envconfig:"MINIO_BUCKET" default:"docs-storage"`

	MinioRegion string `envconfig:"MINIO_REGION" default:"us-east-1"`

	RedisAddr   string `envconfig:"REDIS_ADDR"`
	HTTPPort    string `envconfig:"HTTP_PORT" default:"8080"`
	MetricsPort string `envconfig:"METRICS_PORT" default:"9100"`

	RabbitURL string `envconfig:"RABBIT_URL"`
	DBDSN     string `envconfig:"DB_DSN"`

	PDFSignURL      string `envconfig:"PDFSIGN_URL"`
	MailerURL       string `envconfig:"MAILER_URL"`
	PublicBaseURL   string `envconfig:"PUBLIC_BASE_URL" default:"http://signer.local"`
	MasterKeyHex    string `envconfig:"MASTER_KEY_HEX"`
	MailerTransport string `envconfig:"MAILER_TRANSPORT" default:"log"`
	MailerLogBody   bool   `envconfig:"MAILER_LOG_BODY" default:"true"`
	SMTPHost        string `envconfig:"SMTP_HOST"`
	SMTPPort        string `envconfig:"SMTP_PORT" default:"587"`
	SMTPUsername    string `envconfig:"SMTP_USERNAME"`
	SMTPPassword    string `envconfig:"SMTP_PASSWORD"`
	SMTPFrom        string `envconfig:"SMTP_FROM"`
	SMTPTLSMode     string `envconfig:"SMTP_TLS_MODE" default:"starttls"`
	SMTPServerName  string `envconfig:"SMTP_SERVER_NAME"`

	HTTPReadHeaderTimeout time.Duration `envconfig:"HTTP_READ_HEADER_TIMEOUT" default:"5s"`
	HTTPReadTimeout       time.Duration `envconfig:"HTTP_READ_TIMEOUT" default:"15s"`
	HTTPWriteTimeout      time.Duration `envconfig:"HTTP_WRITE_TIMEOUT" default:"120s"`
	HTTPIdleTimeout       time.Duration `envconfig:"HTTP_IDLE_TIMEOUT" default:"60s"`
	ShutdownTimeout       time.Duration `envconfig:"SHUTDOWN_TIMEOUT" default:"15s"`
	DependencyTimeout     time.Duration `envconfig:"DEPENDENCY_TIMEOUT" default:"30s"`
	PDFSignTimeout        time.Duration `envconfig:"PDFSIGN_TIMEOUT" default:"60s"`

	UploadMaxBytes int64 `envconfig:"UPLOAD_MAX_BYTES" default:"10485760"`
	JSONMaxBytes   int64 `envconfig:"JSON_MAX_BYTES" default:"1048576"`
}

func Load() (*Config, error) {
	var cfg Config
	err := envconfig.Process("", &cfg)
	return &cfg, err
}
