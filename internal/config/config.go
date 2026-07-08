package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	// OllamaURL is the raw OLLAMA_URL value, empty when unset (no default:
	// BE-6 removed the legacy localhost fallback together with its last
	// consumers). It feeds node synthesis (internal/nodesource) and the
	// admin config snapshot only; nothing treats it as a routable backend
	// directly anymore.
	OllamaURL string
	// OllamaURLSet records whether OLLAMA_URL was explicitly present (and
	// non-empty) in the environment. Node synthesis keys off explicit
	// presence, not the value: when unset there is NO implicit localhost
	// node — a fresh install starts with zero nodes. See
	// docs/design/distributed-nodes.md "Backward compatibility with
	// OLLAMA_URL".
	OllamaURLSet bool
	// ModelsListAll (MODELS_LIST_ALL, default false) makes GET /v1/models
	// list every actively priced model regardless of node availability
	// instead of the priced-AND-served intersection.
	ModelsListAll bool
	// NodesFile is the optional path to a JSON node-declaration file
	// (NODES_FILE). Empty means no file is loaded.
	NodesFile           string
	AdminKey            string
	AdminBootstrapToken string
	DatabaseURL         string
	Port                string
	CORSOrigins         string
	MaxRequestBody      int64
	MaxJSONBody         int64
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

	// Cap for the JSON API endpoints (auth/users/accounts/admin). The chat
	// proxy path keeps the larger MAX_REQUEST_BODY cap.
	maxJSONBody := int64(1048576) // 1MB
	if v := os.Getenv("MAX_JSON_REQUEST_BODY"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("invalid MAX_JSON_REQUEST_BODY: %w", err)
		}
		if n <= 0 {
			return Config{}, fmt.Errorf("invalid MAX_JSON_REQUEST_BODY: must be positive, got %d", n)
		}
		maxJSONBody = n
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

	// Track explicit presence of OLLAMA_URL (empty counts as unset, matching
	// the rest of the config): node synthesis must distinguish "operator
	// pointed us at an Ollama" from "nothing configured", so it keys off
	// OllamaURLSet — never the value. There is no default: an unset
	// OLLAMA_URL means a zero-node install (chat 503s until a node is
	// registered; the admin API stays available).
	ollamaURL, ollamaSet := os.LookupEnv("OLLAMA_URL")
	ollamaExplicit := ollamaSet && ollamaURL != ""
	if !ollamaExplicit {
		ollamaURL = ""
	}

	modelsListAll := false
	if v := os.Getenv("MODELS_LIST_ALL"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("invalid MODELS_LIST_ALL: %w", err)
		}
		modelsListAll = b
	}

	return Config{
		OllamaURL:           ollamaURL,
		OllamaURLSet:        ollamaExplicit,
		ModelsListAll:       modelsListAll,
		NodesFile:           os.Getenv("NODES_FILE"),
		AdminKey:            adminKey,
		AdminBootstrapToken: os.Getenv("ADMIN_BOOTSTRAP_TOKEN"),
		DatabaseURL:         databaseURL,
		Port:                envOrDefault("PORT", "8080"),
		CORSOrigins:         envOrDefault("CORS_ORIGINS", "*"),
		MaxRequestBody:      maxBody,
		MaxJSONBody:         maxJSONBody,
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
