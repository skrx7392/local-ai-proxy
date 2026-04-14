package admin

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/krishna/local-ai-proxy/internal/health"
)

// ConfigSnapshot is the whitelisted view of runtime configuration exposed
// at GET /api/admin/config. Secrets (admin_key, database_url,
// admin_bootstrap_token) are deliberately absent — adding fields here is the
// only way they reach the wire.
type ConfigSnapshot struct {
	OllamaURL                string  `json:"ollama_url"`
	Port                     string  `json:"port"`
	LogLevel                 string  `json:"log_level"`
	MaxRequestBodyBytes      int64   `json:"max_request_body_bytes"`
	DefaultCreditGrant       float64 `json:"default_credit_grant"`
	CORSOrigins              string  `json:"cors_origins"`
	AdminRateLimitPerMinute  int     `json:"admin_rate_limit_per_minute"`
	UsageChannelCapacity     int     `json:"usage_channel_capacity"`
	AdminSessionDurationHrs  int     `json:"admin_session_duration_hours"`
	UserSessionDurationHrs   int     `json:"user_session_duration_hours"`
	Version                  string  `json:"version"`
	BuildTime                string  `json:"build_time"`
	GoVersion                string  `json:"go_version"`
}

func (h *handler) getConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.configSnapshot)
}

func (h *handler) getHealth(w http.ResponseWriter, r *http.Request) {
	var (
		allOK  = true
		checks map[string]health.CheckResult
	)
	if h.healthChecker != nil {
		allOK, checks = h.healthChecker.RunChecks(r.Context())
	}
	if checks == nil {
		checks = map[string]health.CheckResult{}
	}

	status := "ok"
	httpStatus := http.StatusOK
	if !allOK {
		status = "degraded"
		httpStatus = http.StatusServiceUnavailable
	}

	uptime := int64(0)
	if !h.startTime.IsZero() {
		uptime = int64(time.Since(h.startTime).Seconds())
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         status,
		"checks":         checks,
		"uptime_seconds": uptime,
		"version":        h.configSnapshot.Version,
	})
}
