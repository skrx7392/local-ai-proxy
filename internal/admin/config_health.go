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
	// OllamaURL reports the raw OLLAMA_URL env value; empty when unset.
	// Since node routing there is no implicit default — the value only
	// documents what (if anything) node synthesis was fed, not a live
	// backend.
	OllamaURL                        string  `json:"ollama_url"`
	Port                             string  `json:"port"`
	LogLevel                         string  `json:"log_level"`
	MaxRequestBodyBytes              int64   `json:"max_request_body_bytes"`
	MaxJSONBodyBytes                 int64   `json:"max_json_request_body_bytes"`
	DefaultCreditGrant               float64 `json:"default_credit_grant"`
	CORSOrigins                      string  `json:"cors_origins"`
	AdminRateLimitPerMinute          int     `json:"admin_rate_limit_per_minute"`
	AuthLoginRateLimitPerMinute      int     `json:"auth_login_rate_limit_per_minute"`
	AuthLoginEmailRateLimitPerMinute int     `json:"auth_login_email_rate_limit_per_minute"`
	AuthRegisterRateLimitPerMinute   int     `json:"auth_register_rate_limit_per_minute"`
	AuthGeneralRateLimitPerMinute    int     `json:"auth_general_rate_limit_per_minute"`
	AuthBcryptMaxConcurrent          int     `json:"auth_bcrypt_max_concurrent"`
	AccountRateLimitPerMinute        int     `json:"account_rate_limit_per_minute"`
	EndUserRateLimitPerMinute        int     `json:"end_user_rate_limit_per_minute"`
	AccountMaxConcurrent             int     `json:"account_max_concurrent"`
	EndUserMaxConcurrent             int     `json:"end_user_max_concurrent"`
	UsageChannelCapacity             int     `json:"usage_channel_capacity"`
	AdminSessionDurationHrs          int     `json:"admin_session_duration_hours"`
	UserSessionDurationHrs           int     `json:"user_session_duration_hours"`
	Version                          string  `json:"version"`
	BuildTime                        string  `json:"build_time"`
	GoVersion                        string  `json:"go_version"`
	ModelsListAll                    bool    `json:"models_list_all"`
	// NodesFile reports the NODES_FILE path; empty when unset. Also used by
	// the nodes API's config-sourced 409 message to point at the file.
	NodesFile string `json:"nodes_file"`
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

	resp := map[string]any{
		"status":         status,
		"checks":         checks,
		"uptime_seconds": uptime,
		"version":        h.configSnapshot.Version,
	}

	// BE 7: per-node breakdown from the registry snapshot, additive to the
	// existing shape. Zero enabled nodes is not degraded (a fresh install
	// must serve the admin API to register its first node) but is flagged.
	if h.nodeRegistry != nil {
		snap := h.nodeRegistry.Snapshot()
		nodes := make([]nodeHealthDTO, 0, len(snap.Nodes))
		for _, ns := range snap.Nodes {
			n := nodeHealthDTO{
				Name:       ns.Node.Name,
				Health:     string(ns.Health),
				LastError:  ns.LastError,
				ModelCount: len(ns.Models),
			}
			if !ns.LastCheckedAt.IsZero() {
				s := ns.LastCheckedAt.Format(time.RFC3339)
				n.LastCheckedAt = &s
			}
			nodes = append(nodes, n)
		}
		resp["nodes"] = nodes
		if len(nodes) == 0 {
			resp["warning"] = "no nodes configured"
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(resp)
}

// nodeHealthDTO is one node's row in the /api/admin/health breakdown.
type nodeHealthDTO struct {
	Name          string  `json:"name"`
	Health        string  `json:"health"`
	LastError     string  `json:"last_error,omitempty"`
	LastCheckedAt *string `json:"last_checked_at"`
	ModelCount    int     `json:"model_count"`
}
