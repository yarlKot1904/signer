package config

import "github.com/kelseyhightower/envconfig"

type Config struct {
	MinioEndpoint string `envconfig:"MINIO_ENDPOINT" required:"true"`
	MinioID       string `envconfig:"MINIO_ID" required:"true"`
	MinioSecret   string `envconfig:"MINIO_SECRET" required:"true"`
	MinioBucket   string `envconfig:"MINIO_BUCKET" default:"docs-storage"`

	MinioRegion string `envconfig:"MINIO_REGION" default:"us-east-1"`

	RedisAddr string `envconfig:"REDIS_ADDR" required:"true"`
	HTTPPort  string `envconfig:"HTTP_PORT" default:"8080"`
}

func Load() (*Config, error) {
	var cfg Config
	err := envconfig.Process("", &cfg)
	return &cfg, err
}
