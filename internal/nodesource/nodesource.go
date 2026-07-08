// Package nodesource declares inference nodes from static startup sources —
// a JSON NODES_FILE and the legacy OLLAMA_URL variable — and reconciles them
// into the store with source='config'.
//
// Merge semantics (docs/design/distributed-nodes.md, "Node registration"):
// declared nodes are upserted by name; config-sourced DB nodes absent from
// the declared set are disabled; API-sourced nodes are never touched; a name
// collision between a declared node and an API-sourced node fails startup.
// Re-running with the same sources is idempotent.
package nodesource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/krishna/local-ai-proxy/internal/config"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// nodesFile is the NODES_FILE document shape. Unknown fields are rejected so
// a typo like "timeout_secs" fails startup instead of being silently dropped.
type nodesFile struct {
	Nodes []declaredNode `json:"nodes"`
}

type declaredNode struct {
	Name           string   `json:"name"`
	BaseURL        string   `json:"base_url"`
	BackendType    string   `json:"backend_type"`
	AuthHeader     *string  `json:"auth_header"`
	StaticModels   []string `json:"static_models"`
	HealthPath     *string  `json:"health_path"`
	TimeoutSeconds *int     `json:"timeout_seconds"`
}

// SyncDeclaredNodes loads the declared node set (NODES_FILE plus OLLAMA_URL
// synthesis) and reconciles it into the store. Any load, validation, or
// collision error is returned before the first write so startup fails fast
// without partially applying the file. BE-6 wires this into main.go right
// after store construction.
func SyncDeclaredNodes(ctx context.Context, s *store.Store, cfg config.Config) error {
	_ = ctx // reserved: store methods manage their own contexts today

	declared, err := loadDeclared(cfg)
	if err != nil {
		return err
	}

	existing, err := s.ListNodesWithSecrets()
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}
	byName := make(map[string]store.Node, len(existing))
	for _, n := range existing {
		byName[n.Name] = n
	}

	// Collision pass before any mutation: a declared name owned by an
	// API-sourced node is a hard error, and nothing may be applied.
	for _, d := range declared {
		if ex, ok := byName[d.Name]; ok && ex.Source != "config" {
			return fmt.Errorf(
				"declared node %q collides with an existing API-registered node (id %d): rename it in NODES_FILE or delete the API node first",
				d.Name, ex.ID)
		}
	}

	declaredNames := make(map[string]bool, len(declared))
	for _, d := range declared {
		declaredNames[d.Name] = true
		if ex, ok := byName[d.Name]; ok {
			d.ID = ex.ID
			if err := s.UpdateNode(d); err != nil {
				return fmt.Errorf("updating declared node %q: %w", d.Name, err)
			}
		} else {
			if _, err := s.CreateNode(d); err != nil {
				return fmt.Errorf("creating declared node %q: %w", d.Name, err)
			}
		}
	}

	// Config-sourced nodes no longer declared anywhere are disabled (never
	// hard-deleted; usage_logs rows reference them).
	for _, ex := range existing {
		if ex.Source == "config" && ex.Enabled && !declaredNames[ex.Name] {
			if err := s.DisableNode(ex.ID); err != nil && !errors.Is(err, store.ErrNodeNotFound) {
				return fmt.Errorf("disabling undeclared config node %q: %w", ex.Name, err)
			}
		}
	}
	return nil
}

// loadDeclared builds the full declared node set: NODES_FILE entries plus,
// when OLLAMA_URL was explicitly set, one synthesized "default" ollama node
// that merges exactly like a file node. When neither source is configured
// the declared set is empty — no implicit localhost node, ever.
func loadDeclared(cfg config.Config) ([]store.Node, error) {
	var declared []store.Node

	if cfg.NodesFile != "" {
		fileNodes, err := loadNodesFile(cfg.NodesFile)
		if err != nil {
			return nil, err
		}
		declared = fileNodes
	}

	if cfg.OllamaURLSet {
		base, err := store.CanonicalizeBaseURL(cfg.OllamaURL)
		if err != nil {
			return nil, fmt.Errorf("OLLAMA_URL: %w", err)
		}
		for _, d := range declared {
			if d.Name == "default" {
				return nil, fmt.Errorf(
					"node %q is declared in %s and also synthesized from OLLAMA_URL: rename the file node or unset OLLAMA_URL",
					"default", cfg.NodesFile)
			}
		}
		declared = append(declared, store.Node{
			Name:        "default",
			BaseURL:     base,
			BackendType: "ollama",
			Enabled:     true,
			Source:      "config",
		})
	}

	return declared, nil
}

// loadNodesFile reads, expands, and validates a NODES_FILE. Every node must
// be valid — one bad entry fails the whole load so startup fails fast.
func loadNodesFile(path string) ([]store.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading NODES_FILE: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f nodesFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parsing NODES_FILE %s: %w", path, err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, fmt.Errorf("parsing NODES_FILE %s: trailing data after JSON document", path)
	}

	nodes := make([]store.Node, 0, len(f.Nodes))
	seen := make(map[string]bool, len(f.Nodes))
	for i, d := range f.Nodes {
		n, err := d.toNode()
		if err != nil {
			return nil, fmt.Errorf("NODES_FILE %s: node %d (%q): %w", path, i, d.Name, err)
		}
		if seen[n.Name] {
			return nil, fmt.Errorf("NODES_FILE %s: duplicate node name %q", path, n.Name)
		}
		seen[n.Name] = true
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// toNode expands ${VAR} references in every string value, then validates the
// declaration using the same exported validators the store applies on write,
// so a bad file entry fails at load time with a per-node error instead of
// mid-merge.
func (d declaredNode) toNode() (store.Node, error) {
	if err := d.expand(); err != nil {
		return store.Node{}, err
	}

	name := strings.TrimSpace(d.Name)
	if name == "" {
		return store.Node{}, errors.New("node name is required")
	}

	base, err := store.CanonicalizeBaseURL(d.BaseURL)
	if err != nil {
		return store.Node{}, err
	}

	backend := d.BackendType
	switch backend {
	case "":
		backend = "ollama"
	case "ollama", "openai_compat":
	default:
		return store.Node{}, fmt.Errorf("backend_type must be ollama or openai_compat, got %q", backend)
	}

	if d.AuthHeader != nil {
		if err := store.ValidateAuthHeader(*d.AuthHeader); err != nil {
			return store.Node{}, err
		}
	}
	if d.HealthPath != nil {
		if err := store.ValidateHealthPath(*d.HealthPath); err != nil {
			return store.Node{}, err
		}
	}
	if d.TimeoutSeconds != nil && *d.TimeoutSeconds <= 0 {
		return store.Node{}, fmt.Errorf("timeout_seconds must be positive, got %d", *d.TimeoutSeconds)
	}

	return store.Node{
		Name:           name,
		BaseURL:        base,
		BackendType:    backend,
		AuthHeader:     d.AuthHeader,
		StaticModels:   d.StaticModels,
		HealthPath:     d.HealthPath,
		TimeoutSeconds: d.TimeoutSeconds,
		Enabled:        true,
		Source:         "config",
	}, nil
}

// expand rewrites ${VAR} references in every string value of d (secrets stay
// out of the file). d has value-receiver callers but pointer fields; string
// fields are rewritten in place via pointers into the local copy.
func (d *declaredNode) expand() error {
	targets := []*string{&d.Name, &d.BaseURL, &d.BackendType, d.AuthHeader, d.HealthPath}
	for i := range d.StaticModels {
		targets = append(targets, &d.StaticModels[i])
	}
	for _, t := range targets {
		if t == nil {
			continue
		}
		v, err := expandEnv(*t)
		if err != nil {
			return err
		}
		*t = v
	}
	return nil
}

// expandEnv substitutes ${VAR} references from the environment. An undefined
// variable is a hard error naming the variable — silently substituting ""
// would hide a broken secret. Only the braced ${VAR} form is recognized; a
// bare $ passes through untouched. Set-but-empty variables substitute the
// empty string (they are defined; downstream validation catches the fallout).
func expandEnv(s string) (string, error) {
	if !strings.Contains(s, "${") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], "${")
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+j])
		rest := s[i+j+2:]
		k := strings.IndexByte(rest, '}')
		if k < 0 {
			return "", fmt.Errorf("unterminated ${ reference in %q", s)
		}
		name := rest[:k]
		if !validEnvName(name) {
			return "", fmt.Errorf("invalid environment variable reference ${%s} in %q", name, s)
		}
		v, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("environment variable %s is referenced but not set", name)
		}
		b.WriteString(v)
		i += j + 2 + k + 1
	}
	return b.String(), nil
}

// validEnvName reports whether name is a POSIX-style variable name:
// [A-Za-z_][A-Za-z0-9_]*.
func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
