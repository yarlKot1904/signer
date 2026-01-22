package config

import "github.com/kelseyhightower/envconfig"

type Config struct {
	MinioEndpoint string `envconfig:"MINIO_ENDPOINT"`
	MinioID       string `envconfig:"MINIO_ID"`
	MinioSecret   string `envconfig:"MINIO_SECRET"`
	MinioBucket   string `envconfig:"MINIO_BUCKET" default:"docs-storage"`

	MinioRegion string `envconfig:"MINIO_REGION" default:"us-east-1"`

	RedisAddr string `envconfig:"REDIS_ADDR"`
	HTTPPort  string `envconfig:"HTTP_PORT" default:"8080"`

	RabbitURL string `envconfig:"RABBIT_URL"`
	DBDSN     string `envconfig:"DB_DSN"`
}

func Load() (*Config, error) {
	var cfg Config
	err := envconfig.Process("", &cfg)
	return &cfg, err
}
