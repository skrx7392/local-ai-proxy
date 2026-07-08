package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNodeNotFound is returned by node-mutation helpers when the target row
// does not exist.
var ErrNodeNotFound = errors.New("node not found")

// ErrNodeNameExists is returned when a create or update would violate the
// unique node name constraint, so callers can map to 409 without inspecting
// SQL errors.
var ErrNodeNameExists = errors.New("node name already exists")

// Node is an inference backend the gateway routes to, identified by a base
// URL. AuthHeader is a secret: GetNode/ListNodes return it masked (see
// maskAuthHeader); only the *WithSecrets variants return the raw value.
type Node struct {
	ID             int64
	Name           string
	BaseURL        string
	BackendType    string   // "ollama" or "openai_compat"
	AuthHeader     *string  // nil = none; masked except in *WithSecrets methods
	StaticModels   []string // non-nil disables model discovery; exact list
	HealthPath     *string  // optional liveness-probe path override (path-only)
	TimeoutSeconds *int     // optional per-node request timeout (nil = default)
	Enabled        bool
	Source         string // "api" or "config"
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// nodeColumns is the shared SELECT list for node queries. auth_header is
// always scanned raw; masking is applied afterwards by the non-secret
// accessors.
const nodeColumns = `id, name, base_url, backend_type, auth_header, static_models,
	health_path, timeout_seconds, enabled, source, created_at, updated_at`

// CanonicalizeBaseURL validates and canonicalizes a node base URL. The
// scheme must be http or https; userinfo, query, and fragment are rejected;
// the scheme and host are lowercased and trailing slashes are trimmed. A
// base_url ending in /v1 is rejected: the gateway path-joins /v1 itself, so
// including it would produce .../v1/v1/... upstream URLs.
func CanonicalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("base_url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("base_url is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("base_url scheme must be http or https, got %q", u.Scheme)
	}
	if u.Opaque != "" {
		return "", errors.New("base_url is not a valid URL")
	}
	if u.Host == "" {
		return "", errors.New("base_url must include a host")
	}
	if u.User != nil {
		return "", errors.New("base_url must not contain userinfo (user:pass@)")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", errors.New("base_url must not contain a query string")
	}
	if u.Fragment != "" {
		return "", errors.New("base_url must not contain a fragment")
	}

	path := strings.TrimRight(u.EscapedPath(), "/")
	if strings.HasSuffix(path, "/v1") {
		return "", fmt.Errorf(
			"base_url must not end with /v1: the gateway appends /v1 itself (use %q)",
			u.Scheme+"://"+strings.ToLower(u.Host)+strings.TrimSuffix(path, "/v1"))
	}
	return u.Scheme + "://" + strings.ToLower(u.Host) + path, nil
}

// ValidateAuthHeader validates an Authorization header value destined for a
// node. Every byte must be printable ASCII (0x20–0x7E): this rejects CR/LF
// (header injection), NUL, tabs, and non-ASCII input such as a masked value
// being written back. Empty values are rejected — omit the header instead.
func ValidateAuthHeader(v string) error {
	if v == "" {
		return errors.New("auth_header must not be empty; omit it instead")
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] > 0x7e {
			return errors.New("auth_header contains control or non-ASCII characters")
		}
	}
	return nil
}

// ValidateHealthPath validates a node health-check path override. It must be
// path-only — starting with "/", no scheme, host, userinfo, query, or
// fragment — so it can only ever be path-joined onto the node's base_url and
// never becomes a second URL/SSRF surface.
func ValidateHealthPath(p string) error {
	if p == "" {
		return errors.New("health_path must not be empty; omit it instead")
	}
	if !strings.HasPrefix(p, "/") {
		return errors.New("health_path must start with /")
	}
	if strings.HasPrefix(p, "//") {
		return errors.New("health_path must not start with // (protocol-relative URL)")
	}
	for i := 0; i < len(p); i++ {
		if p[i] <= 0x20 || p[i] > 0x7e {
			return errors.New("health_path contains whitespace, control, or non-ASCII characters")
		}
	}
	u, err := url.Parse(p)
	if err != nil {
		return fmt.Errorf("health_path is not a valid path: %w", err)
	}
	if u.Scheme != "" || u.Host != "" || u.User != nil {
		return errors.New("health_path must be a path only, without scheme or host")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return errors.New("health_path must not contain a query string")
	}
	if u.Fragment != "" {
		return errors.New("health_path must not contain a fragment")
	}
	return nil
}

// maskAuthHeader returns a display-safe form of an auth header value, e.g.
// "Bearer sk-…abcd" — a short prefix plus the last 4 characters, the same
// spirit as api_keys.key_prefix. Values too short to mask meaningfully
// collapse to "…".
func maskAuthHeader(v string) string {
	const prefixLen, suffixLen = 10, 4
	if len(v) <= prefixLen+suffixLen {
		return "…"
	}
	return v[:prefixLen] + "…" + v[len(v)-suffixLen:]
}

// validateNode normalizes and validates the caller-supplied fields of n,
// returning the canonical base_url. Shared by CreateNode and UpdateNode so
// invalid nodes can never be persisted regardless of caller.
func validateNode(n *Node) error {
	n.Name = strings.TrimSpace(n.Name)
	if n.Name == "" {
		return errors.New("node name is required")
	}

	canonical, err := CanonicalizeBaseURL(n.BaseURL)
	if err != nil {
		return err
	}
	n.BaseURL = canonical

	switch n.BackendType {
	case "":
		n.BackendType = "ollama"
	case "ollama", "openai_compat":
	default:
		return fmt.Errorf("backend_type must be ollama or openai_compat, got %q", n.BackendType)
	}

	if n.AuthHeader != nil {
		if err := ValidateAuthHeader(*n.AuthHeader); err != nil {
			return err
		}
	}
	if n.HealthPath != nil {
		if err := ValidateHealthPath(*n.HealthPath); err != nil {
			return err
		}
	}
	if n.TimeoutSeconds != nil && *n.TimeoutSeconds <= 0 {
		return fmt.Errorf("timeout_seconds must be positive, got %d", *n.TimeoutSeconds)
	}
	return nil
}

// ValidateNode normalizes and validates the caller-supplied fields of n in
// place (trims the name, canonicalizes base_url, defaults backend_type, and
// checks auth_header/health_path/timeout_seconds). Exported so admin handlers
// can map validation failures to 400 with the validator's message before
// attempting a write; CreateNode and UpdateNode run the same validation
// internally, so skipping this can never persist an invalid node.
func ValidateNode(n *Node) error {
	return validateNode(n)
}

// CreateNode validates, canonicalizes, and inserts a new node, returning its
// ID. BackendType defaults to "ollama" and Source to "api" when empty; new
// nodes are always created enabled. Returns ErrNodeNameExists on a name
// collision.
func (s *Store) CreateNode(n Node) (int64, error) {
	if err := validateNode(&n); err != nil {
		return 0, err
	}
	switch n.Source {
	case "":
		n.Source = "api"
	case "api", "config":
	default:
		return 0, fmt.Errorf("source must be api or config, got %q", n.Source)
	}

	var id int64
	err := s.pool.QueryRow(
		context.Background(),
		`INSERT INTO nodes (name, base_url, backend_type, auth_header, static_models, health_path, timeout_seconds, source)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		n.Name, n.BaseURL, n.BackendType, n.AuthHeader, n.StaticModels, n.HealthPath, n.TimeoutSeconds, n.Source,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrNodeNameExists
		}
		return 0, err
	}
	return id, nil
}

// GetNode looks up a node by ID with auth_header MASKED — safe for admin
// handlers and listings. Use GetNodeWithSecrets only where the raw secret is
// genuinely needed (registry loader, upstream probes). Returns (nil, nil)
// when the node does not exist.
func (s *Store) GetNode(id int64) (*Node, error) {
	n, err := s.getNodeRaw(id)
	if err != nil || n == nil {
		return n, err
	}
	maskNodeSecret(n)
	return n, nil
}

// GetNodeWithSecrets looks up a node by ID with the RAW auth_header. For
// internal consumers (registry loader / probes) only — never expose the
// result on an admin or API response. Returns (nil, nil) when the node does
// not exist.
func (s *Store) GetNodeWithSecrets(id int64) (*Node, error) {
	return s.getNodeRaw(id)
}

func (s *Store) getNodeRaw(id int64) (*Node, error) {
	var n Node
	err := s.pool.QueryRow(
		context.Background(),
		`SELECT `+nodeColumns+` FROM nodes WHERE id = $1`, id,
	).Scan(&n.ID, &n.Name, &n.BaseURL, &n.BackendType, &n.AuthHeader, &n.StaticModels,
		&n.HealthPath, &n.TimeoutSeconds, &n.Enabled, &n.Source, &n.CreatedAt, &n.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// ListNodes returns all nodes ordered by ID with auth_header MASKED — safe
// for admin handlers and listings. Use ListNodesWithSecrets only where the
// raw secret is genuinely needed.
func (s *Store) ListNodes() ([]Node, error) {
	nodes, err := s.listNodesRaw()
	if err != nil {
		return nil, err
	}
	for i := range nodes {
		maskNodeSecret(&nodes[i])
	}
	return nodes, nil
}

// ListNodesWithSecrets returns all nodes ordered by ID with the RAW
// auth_header. For internal consumers (registry loader / probes) only —
// never expose the result on an admin or API response.
func (s *Store) ListNodesWithSecrets() ([]Node, error) {
	return s.listNodesRaw()
}

func (s *Store) listNodesRaw() ([]Node, error) {
	rows, err := s.pool.Query(
		context.Background(),
		`SELECT `+nodeColumns+` FROM nodes ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.Name, &n.BaseURL, &n.BackendType, &n.AuthHeader, &n.StaticModels,
			&n.HealthPath, &n.TimeoutSeconds, &n.Enabled, &n.Source, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

func maskNodeSecret(n *Node) {
	if n.AuthHeader != nil {
		masked := maskAuthHeader(*n.AuthHeader)
		n.AuthHeader = &masked
	}
}

// UpdateNode validates and rewrites the mutable fields of the node with
// n.ID (name, base_url, backend_type, auth_header, static_models,
// health_path, timeout_seconds, enabled) and sets updated_at. Source is
// fixed at creation and never updated. A nil AuthHeader clears the stored
// secret — callers doing read-modify-write must load via GetNodeWithSecrets,
// not GetNode, or they would write the masked value back (rejected by
// validation, which only accepts printable ASCII).
func (s *Store) UpdateNode(n Node) error {
	if err := validateNode(&n); err != nil {
		return err
	}

	ct, err := s.pool.Exec(
		context.Background(),
		`UPDATE nodes
		 SET name = $1, base_url = $2, backend_type = $3, auth_header = $4, static_models = $5,
		     health_path = $6, timeout_seconds = $7, enabled = $8, updated_at = NOW()
		 WHERE id = $9`,
		n.Name, n.BaseURL, n.BackendType, n.AuthHeader, n.StaticModels,
		n.HealthPath, n.TimeoutSeconds, n.Enabled, n.ID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrNodeNameExists
		}
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNodeNotFound
	}
	return nil
}

// DisableNode soft-deletes a node by setting enabled = FALSE. Nodes are
// never hard-deleted because usage_logs rows reference them.
func (s *Store) DisableNode(id int64) error {
	ct, err := s.pool.Exec(
		context.Background(),
		`UPDATE nodes SET enabled = FALSE, updated_at = NOW() WHERE id = $1`, id,
	)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNodeNotFound
	}
	return nil
}
