package config

import (
	"fmt"
	"math"
	"net/url"
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

	// AdminServiceCreditGrant is the credit balance the auto-created
	// "admin-service" account starts with (applied once, at creation).
	// Generous by default so upgrading deployments' legacy admin keys keep
	// working; operators can lower it via ADMIN_SERVICE_CREDIT_GRANT.
	AdminServiceCreditGrant float64

	// EndUserMonthlyGrant (END_USER_MONTHLY_GRANT, default 5.0) is the
	// monthly allowance for auto-provisioned end-user accounts, overridable
	// per account via accounts.monthly_grant. See
	// docs/design/end-user-accounts.md.
	EndUserMonthlyGrant float64

	// CreditAlertWebhookURL (CREDIT_ALERT_WEBHOOK_URL, optional) receives a
	// POST when an end-user account hits its monthly allowance
	// (docs/design/credit-requests.md). Empty = notifications disabled;
	// cap-hits are still recorded in credit_requests.
	CreditAlertWebhookURL string

	// Public auth-surface rate limits (requests per minute) and the global
	// bcrypt concurrency cap. See internal/authlimit.
	AuthLoginPerMinIP     int
	AuthLoginPerMinEmail  int
	AuthRegisterPerMinIP  int
	AuthGeneralPerMinIP   int
	AuthBcryptConcurrency int

	// Account-level chat rate limits (requests per minute), by account
	// class; per-account override via accounts.rate_limit_per_min. There is
	// no 0=disabled semantic — to effectively disable, set the max. See
	// docs/design/per-account-rate-limiting.md.
	AccountRateLimitPerMin int // ACCOUNT_RATELIMIT_PER_MIN, service accounts
	EndUserRateLimitPerMin int // END_USER_RATELIMIT_PER_MIN, allowance-managed accounts

	// Per-account caps on in-flight non-GET requests (the control that
	// bounds GPU occupancy; requests/min only bounds arrival rate).
	AccountMaxConcurrent int // ACCOUNT_MAX_CONCURRENT, service accounts
	EndUserMaxConcurrent int // END_USER_MAX_CONCURRENT, allowance-managed accounts
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

	// Initial balance for the auto-created admin service account. Defaults
	// high (1M credits) so admin/smoke-test keys work out of the box; a
	// negative value is a configuration error.
	adminServiceCreditGrant := float64(1000000)
	if v := os.Getenv("ADMIN_SERVICE_CREDIT_GRANT"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return Config{}, fmt.Errorf("invalid ADMIN_SERVICE_CREDIT_GRANT: %w", err)
		}
		if n < 0 {
			return Config{}, fmt.Errorf("invalid ADMIN_SERVICE_CREDIT_GRANT: must be >= 0, got %v", n)
		}
		adminServiceCreditGrant = n
	}

	// Monthly allowance for auto-provisioned end-user accounts. Zero is a
	// valid operator choice (new end users start blocked until an admin sets
	// a per-account override); negative is a configuration error.
	endUserMonthlyGrant := 5.0
	if v := os.Getenv("END_USER_MONTHLY_GRANT"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return Config{}, fmt.Errorf("invalid END_USER_MONTHLY_GRANT: %w", err)
		}
		// NaN poisons every downstream comparison (NaN comparisons are all
		// false → the credit gate and reserve check never trip → unlimited
		// spend); infinity is the same defect spelled differently.
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			return Config{}, fmt.Errorf("invalid END_USER_MONTHLY_GRANT: must be a finite value >= 0, got %v", n)
		}
		endUserMonthlyGrant = n
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

	// Account-level chat rate limits. Defaults: 300/min for service accounts
	// (generous — existing multi-key accounts mostly don't break on day one,
	// but finally bounded), 30/min for end users (Open WebUI fires 2–4
	// upstream completions per visible message, so 30 ≈ 7–10 messages/min).
	accountRateLimit, err := boundedIntEnvOrDefault("ACCOUNT_RATELIMIT_PER_MIN", 300, maxRateLimitPerMin)
	if err != nil {
		return Config{}, err
	}
	endUserRateLimit, err := boundedIntEnvOrDefault("END_USER_RATELIMIT_PER_MIN", 30, maxRateLimitPerMin)
	if err != nil {
		return Config{}, err
	}
	// Concurrency caps: 5 for end users (one visible Open WebUI message =
	// 1 visible stream + 2–4 parallel background completions, so 3 can trip
	// on a single send), 8 for service accounts.
	accountMaxConcurrent, err := boundedIntEnvOrDefault("ACCOUNT_MAX_CONCURRENT", 8, maxConcurrentCap)
	if err != nil {
		return Config{}, err
	}
	endUserMaxConcurrent, err := boundedIntEnvOrDefault("END_USER_MAX_CONCURRENT", 5, maxConcurrentCap)
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

	// A malformed webhook URL would fail silently on every cap-hit, so
	// reject it at boot instead of at notification time.
	creditAlertWebhookURL := os.Getenv("CREDIT_ALERT_WEBHOOK_URL")
	if creditAlertWebhookURL != "" {
		u, err := url.Parse(creditAlertWebhookURL)
		if err != nil {
			return Config{}, fmt.Errorf("invalid CREDIT_ALERT_WEBHOOK_URL: %w", err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Config{}, fmt.Errorf("invalid CREDIT_ALERT_WEBHOOK_URL: must be an absolute http(s) URL, got %q", creditAlertWebhookURL)
		}
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

		AdminServiceCreditGrant: adminServiceCreditGrant,
		EndUserMonthlyGrant:     endUserMonthlyGrant,
		CreditAlertWebhookURL:   creditAlertWebhookURL,

		AuthLoginPerMinIP:     authLoginIP,
		AuthLoginPerMinEmail:  authLoginEmail,
		AuthRegisterPerMinIP:  authRegisterIP,
		AuthGeneralPerMinIP:   authGeneralIP,
		AuthBcryptConcurrency: bcryptConcurrency,

		AccountRateLimitPerMin: accountRateLimit,
		EndUserRateLimitPerMin: endUserRateLimit,
		AccountMaxConcurrent:   accountMaxConcurrent,
		EndUserMaxConcurrent:   endUserMaxConcurrent,
	}, nil
}

// maxRateLimitPerMin mirrors ratelimit.MaxConfigPerMinute (typo guard) —
// duplicated here to keep config free of the ratelimit package's HTTP deps.
const maxRateLimitPerMin = 10000

// maxConcurrentCap bounds the per-account concurrency caps (typo guard).
const maxConcurrentCap = 100

// boundedIntEnvOrDefault is intEnvOrDefault with an upper bound.
func boundedIntEnvOrDefault(key string, fallback, max int) (int, error) {
	n, err := intEnvOrDefault(key, fallback)
	if err != nil {
		return 0, err
	}
	if n > max {
		return 0, fmt.Errorf("invalid %s: must be <= %d, got %d", key, max, n)
	}
	return n, nil
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
