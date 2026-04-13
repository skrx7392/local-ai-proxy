package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	OllamaURL           string
	AdminKey            string
	AdminBootstrapToken string
	DatabaseURL         string
	Port                string
	CORSOrigins         string
	MaxRequestBody      int64
	DefaultCreditGrant  float64
	LogLevel            string
}

func Load() (Config, error) {
	adminKey := os.Getenv("ADMIN_KEY")
	if adminKey == "" {
		return Config{}, fmt.Errorf("ADMIN_KEY environment variable is required")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL environment variable is required")
	}

	maxBody := int64(52428800) // 50MB
	if v := os.Getenv("MAX_REQUEST_BODY"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("invalid MAX_REQUEST_BODY: %w", err)
		}
		maxBody = n
	}

	var defaultCreditGrant float64
	if v := os.Getenv("DEFAULT_CREDIT_GRANT"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return Config{}, fmt.Errorf("invalid DEFAULT_CREDIT_GRANT: %w", err)
		}
		defaultCreditGrant = n
	}

	return Config{
		OllamaURL:           envOrDefault("OLLAMA_URL", "http://localhost:11434"),
		AdminKey:            adminKey,
		AdminBootstrapToken: os.Getenv("ADMIN_BOOTSTRAP_TOKEN"),
		DatabaseURL:         databaseURL,
		Port:                envOrDefault("PORT", "8080"),
		CORSOrigins:         envOrDefault("CORS_ORIGINS", "*"),
		MaxRequestBody:      maxBody,
		DefaultCreditGrant:  defaultCreditGrant,
		LogLevel:            envOrDefault("LOG_LEVEL", "info"),
	}, nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
