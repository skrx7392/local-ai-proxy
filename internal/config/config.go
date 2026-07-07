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

	// Public auth-surface rate limits (requests per minute) and the global
	// bcrypt concurrency cap. See internal/authlimit.
	AuthLoginPerMinIP     int
	AuthLoginPerMinEmail  int
	AuthRegisterPerMinIP  int
	AuthGeneralPerMinIP   int
	AuthBcryptConcurrency int
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

	authLoginIP, err := intEnvOrDefault("AUTH_RATELIMIT_LOGIN_PER_MIN", 5)
	if err != nil {
		return Config{}, err
	}
	authLoginEmail, err := intEnvOrDefault("AUTH_RATELIMIT_LOGIN_EMAIL_PER_MIN", 5)
	if err != nil {
		return Config{}, err
	}
	authRegisterIP, err := intEnvOrDefault("AUTH_RATELIMIT_REGISTER_PER_MIN", 3)
	if err != nil {
		return Config{}, err
	}
	authGeneralIP, err := intEnvOrDefault("AUTH_RATELIMIT_GENERAL_PER_MIN", 120)
	if err != nil {
		return Config{}, err
	}
	bcryptConcurrency, err := intEnvOrDefault("AUTH_BCRYPT_MAX_CONCURRENT", 8)
	if err != nil {
		return Config{}, err
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

		AuthLoginPerMinIP:     authLoginIP,
		AuthLoginPerMinEmail:  authLoginEmail,
		AuthRegisterPerMinIP:  authRegisterIP,
		AuthGeneralPerMinIP:   authGeneralIP,
		AuthBcryptConcurrency: bcryptConcurrency,
	}, nil
}

// intEnvOrDefault parses a positive integer env var, returning fallback when
// unset. Zero, negative, and non-numeric values are configuration errors —
// silently disabling a security limit is worse than failing to boot.
func intEnvOrDefault(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("invalid %s: must be a positive integer, got %d", key, n)
	}
	return n, nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
