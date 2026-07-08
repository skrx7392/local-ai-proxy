package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/krishna/local-ai-proxy/internal/apierror"
	"github.com/krishna/local-ai-proxy/internal/proxy"
	"github.com/krishna/local-ai-proxy/internal/registry"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// BE 7: backend node management. Every response goes through nodeDTO built
// from the store's MASKED reads (GetNode/ListNodes) — the raw auth_header can
// never reach the wire — joined with live state from the registry snapshot.
// Mutations call the poller's RefreshNode synchronously, so a node is probed
// and routable (or removed from routing) before the HTTP response returns.

// nodeDTO is the wire shape of a node on all /api/admin/nodes responses:
// masked store config plus live registry state. FE-1 renders this directly.
type nodeDTO struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	BaseURL     string `json:"base_url"`
	BackendType string `json:"backend_type"`
	// AuthHeader is MASKED (e.g. "Bearer sk-…abcd"); null = no auth header.
	AuthHeader *string `json:"auth_header"`
	// StaticModels null = model list is discovered by probing.
	StaticModels   []string `json:"static_models"`
	HealthPath     *string  `json:"health_path"`
	TimeoutSeconds *int     `json:"timeout_seconds"`
	Enabled        bool     `json:"enabled"`
	Source         string   `json:"source"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`

	// Live state from the registry snapshot. Nodes the registry doesn't know
	// (disabled, or created before any probe) read health "unknown" with an
	// empty model list.
	Health        string   `json:"health"`
	Models        []string `json:"models"`
	LastError     string   `json:"last_error,omitempty"`
	LastCheckedAt *string  `json:"last_checked_at"`
}

// createNodeRequest is the POST /api/admin/nodes body (design doc, Admin API).
type createNodeRequest struct {
	Name           string   `json:"name"`
	BaseURL        string   `json:"base_url"`
	BackendType    string   `json:"backend_type"`
	AuthHeader     *string  `json:"auth_header"`
	StaticModels   []string `json:"static_models"`
	HealthPath     *string  `json:"health_path"`
	TimeoutSeconds *int     `json:"timeout_seconds"`
}

// updateNodeRequest is the PUT /api/admin/nodes/{id} body. Every field is
// optional; omitted fields keep their current value (PATCH-like semantics,
// chosen because UpdateNode is a full-row write and a masked read must never
// round-trip into it):
//
//   - auth_header: absent (or JSON null) = keep, "" = clear, value = replace.
//   - health_path: absent = keep, "" = clear, value = replace.
//   - static_models: absent (or null) = keep, [] = clear (switch the node
//     back to probe discovery), non-empty = replace.
//   - timeout_seconds: absent = keep, 0 = clear (use the default timeout),
//     positive = replace.
//   - enabled: absent = keep; true re-enables a previously deleted node.
type updateNodeRequest struct {
	Name           *string   `json:"name"`
	BaseURL        *string   `json:"base_url"`
	BackendType    *string   `json:"backend_type"`
	AuthHeader     *string   `json:"auth_header"`
	StaticModels   *[]string `json:"static_models"`
	HealthPath     *string   `json:"health_path"`
	TimeoutSeconds *int      `json:"timeout_seconds"`
	Enabled        *bool     `json:"enabled"`
}

// nodeLiveStates indexes the registry snapshot by node ID. A nil registry
// (zero Options) behaves as an empty snapshot.
func (h *handler) nodeLiveStates() map[int64]registry.NodeState {
	if h.nodeRegistry == nil {
		return nil
	}
	snap := h.nodeRegistry.Snapshot()
	states := make(map[int64]registry.NodeState, len(snap.Nodes))
	for _, ns := range snap.Nodes {
		states[ns.Node.ID] = ns
	}
	return states
}

// toNodeDTO joins a MASKED store node with its live registry state. Callers
// must never pass a node loaded via GetNodeWithSecrets.
func toNodeDTO(n *store.Node, states map[int64]registry.NodeState) nodeDTO {
	dto := nodeDTO{
		ID:             n.ID,
		Name:           n.Name,
		BaseURL:        n.BaseURL,
		BackendType:    n.BackendType,
		AuthHeader:     n.AuthHeader,
		StaticModels:   n.StaticModels,
		HealthPath:     n.HealthPath,
		TimeoutSeconds: n.TimeoutSeconds,
		Enabled:        n.Enabled,
		Source:         n.Source,
		CreatedAt:      n.CreatedAt.Format(time.RFC3339),
		UpdatedAt:      n.UpdatedAt.Format(time.RFC3339),
		Health:         string(registry.HealthUnknown),
		Models:         []string{},
	}
	ns, ok := states[n.ID]
	if !ok {
		return dto
	}
	dto.Health = string(ns.Health)
	if ns.Models != nil {
		dto.Models = ns.Models
	}
	dto.LastError = ns.LastError
	if !ns.LastCheckedAt.IsZero() {
		s := ns.LastCheckedAt.Format(time.RFC3339)
		dto.LastCheckedAt = &s
	}
	return dto
}

// runNodeRefresh forces a synchronous reload + probe of one node. A nil
// refresher (zero Options) is a no-op. The returned error is a failed DB
// reload only; probe failures land in the registry as live state.
func (h *handler) runNodeRefresh(r *http.Request, nodeID int64) error {
	if h.nodeRefresher == nil {
		return nil
	}
	return h.nodeRefresher.RefreshNode(r.Context(), nodeID)
}

// configSourcedNodeError writes the 409 for mutations of config-sourced
// nodes, pointing the admin at the nodes file that owns them.
func (h *handler) configSourcedNodeError(w http.ResponseWriter, r *http.Request) {
	msg := "Node is config-sourced and read-only via the API; edit the nodes file (NODES_FILE) and restart"
	if path := h.configSnapshot.NodesFile; path != "" {
		msg = "Node is config-sourced and read-only via the API; edit " + path + " (NODES_FILE) and restart"
	}
	proxy.WriteError(w, r, http.StatusConflict, "config_sourced_node", "invalid_request_error", msg)
}

// writeNodeResponse emits the standard {data: nodeDTO} envelope, reloading
// the node (masked) and its live state so the client always sees post-refresh
// truth.
func (h *handler) writeNodeResponse(w http.ResponseWriter, r *http.Request, nodeID int64, status int) {
	n, err := h.store.GetNode(nodeID)
	if err != nil || n == nil {
		slog.ErrorContext(r.Context(), "reload node for response", "error", err, "node_id", nodeID)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to reload node")
		return
	}
	dto := toNodeDTO(n, h.nodeLiveStates())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Envelope{Data: dto})
}

// createNode handles POST /api/admin/nodes: validate via the store's node
// validators, insert, probe synchronously, and return 201 with the masked
// node plus its initial live state.
func (h *handler) createNode(w http.ResponseWriter, r *http.Request) {
	var req createNodeRequest
	if !apierror.DecodeJSON(w, r, &req) {
		return
	}

	n := store.Node{
		Name:           req.Name,
		BaseURL:        req.BaseURL,
		BackendType:    req.BackendType,
		AuthHeader:     req.AuthHeader,
		StaticModels:   req.StaticModels,
		HealthPath:     req.HealthPath,
		TimeoutSeconds: req.TimeoutSeconds,
	}
	if err := store.ValidateNode(&n); err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_node", "invalid_request_error", err.Error())
		return
	}

	id, err := h.store.CreateNode(n)
	if err != nil {
		if errors.Is(err, store.ErrNodeNameExists) {
			proxy.WriteError(w, r, http.StatusConflict, "node_name_exists", "invalid_request_error", "A node with this name already exists")
			return
		}
		slog.ErrorContext(r.Context(), "create node error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to create node")
		return
	}

	// Initial synchronous probe (design doc: nodes created via the API are
	// probed immediately so the response includes initial health). A failed
	// reload is logged, not fatal — the node exists; live state stays unknown
	// until the poller reaches it.
	if err := h.runNodeRefresh(r, id); err != nil {
		slog.ErrorContext(r.Context(), "initial node refresh error", "error", err, "node_id", id)
	}
	h.writeNodeResponse(w, r, id, http.StatusCreated)
}

// listNodes handles GET /api/admin/nodes: all nodes (masked) joined with live
// state from one registry snapshot.
func (h *handler) listNodes(w http.ResponseWriter, r *http.Request) {
	envelope, ecode, emsg, eerr := wantEnvelope(r)
	if eerr != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, ecode, "invalid_request_error", emsg)
		return
	}
	limit, offset, pcode, pmsg, perr := parsePagination(r)
	if perr != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, pcode, "invalid_request_error", pmsg)
		return
	}

	nodes, err := h.store.ListNodes()
	if err != nil {
		slog.ErrorContext(r.Context(), "list nodes error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to list nodes")
		return
	}

	states := h.nodeLiveStates()
	resp := make([]nodeDTO, 0, len(nodes))
	for i := range nodes {
		resp = append(resp, toNodeDTO(&nodes[i], states))
	}

	if envelope {
		page, pag := sliceWindow(resp, limit, offset)
		writeEnvelope(w, page, pag)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// getNode handles GET /api/admin/nodes/{id}.
func (h *handler) getNode(w http.ResponseWriter, r *http.Request) {
	id, ok := h.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	n, err := h.store.GetNode(id)
	if err != nil {
		slog.ErrorContext(r.Context(), "get node error", "error", err, "node_id", id)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to get node")
		return
	}
	if n == nil {
		proxy.WriteError(w, r, http.StatusNotFound, "not_found", "invalid_request_error", "Node not found")
		return
	}
	writeEnvelope(w, toNodeDTO(n, h.nodeLiveStates()), nil)
}

// updateNode handles PUT /api/admin/nodes/{id}: PATCH-like update of the
// mutable fields (see updateNodeRequest for the keep/clear/replace rules),
// then a synchronous re-probe. Config-sourced nodes are read-only → 409.
func (h *handler) updateNode(w http.ResponseWriter, r *http.Request) {
	id, ok := h.nodeIDFromPath(w, r)
	if !ok {
		return
	}

	// Read-modify-write MUST load the raw secret: UpdateNode is a full-row
	// write where nil AuthHeader clears the stored value, and writing a
	// masked value back is rejected by validation.
	current, err := h.store.GetNodeWithSecrets(id)
	if err != nil {
		slog.ErrorContext(r.Context(), "load node for update", "error", err, "node_id", id)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to load node")
		return
	}
	if current == nil {
		proxy.WriteError(w, r, http.StatusNotFound, "not_found", "invalid_request_error", "Node not found")
		return
	}
	if current.Source == "config" {
		h.configSourcedNodeError(w, r)
		return
	}

	var req updateNodeRequest
	if !apierror.DecodeJSON(w, r, &req) {
		return
	}

	if req.Name != nil {
		current.Name = *req.Name
	}
	if req.BaseURL != nil {
		current.BaseURL = *req.BaseURL
	}
	if req.BackendType != nil {
		current.BackendType = *req.BackendType
	}
	if req.AuthHeader != nil {
		if *req.AuthHeader == "" {
			current.AuthHeader = nil // "" = clear the stored secret
		} else {
			current.AuthHeader = req.AuthHeader
		}
	}
	if req.StaticModels != nil {
		if len(*req.StaticModels) == 0 {
			current.StaticModels = nil // [] = back to probe discovery
		} else {
			current.StaticModels = *req.StaticModels
		}
	}
	if req.HealthPath != nil {
		if *req.HealthPath == "" {
			current.HealthPath = nil // "" = clear the override
		} else {
			current.HealthPath = req.HealthPath
		}
	}
	if req.TimeoutSeconds != nil {
		if *req.TimeoutSeconds == 0 {
			current.TimeoutSeconds = nil // 0 = back to the default timeout
		} else {
			current.TimeoutSeconds = req.TimeoutSeconds
		}
	}
	if req.Enabled != nil {
		current.Enabled = *req.Enabled
	}

	if err := store.ValidateNode(current); err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_node", "invalid_request_error", err.Error())
		return
	}
	if err := h.store.UpdateNode(*current); err != nil {
		switch {
		case errors.Is(err, store.ErrNodeNameExists):
			proxy.WriteError(w, r, http.StatusConflict, "node_name_exists", "invalid_request_error", "A node with this name already exists")
		case errors.Is(err, store.ErrNodeNotFound):
			proxy.WriteError(w, r, http.StatusNotFound, "not_found", "invalid_request_error", "Node not found")
		default:
			slog.ErrorContext(r.Context(), "update node error", "error", err, "node_id", id)
			proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to update node")
		}
		return
	}

	if err := h.runNodeRefresh(r, id); err != nil {
		slog.ErrorContext(r.Context(), "node refresh after update", "error", err, "node_id", id)
	}
	h.writeNodeResponse(w, r, id, http.StatusOK)
}

// deleteNode handles DELETE /api/admin/nodes/{id}: soft-delete (disable) and
// synchronously drop the node from routing. Nodes are never hard-deleted —
// usage_logs rows reference them. Config-sourced nodes → 409 (remove them
// from the nodes file instead).
func (h *handler) deleteNode(w http.ResponseWriter, r *http.Request) {
	id, ok := h.nodeIDFromPath(w, r)
	if !ok {
		return
	}

	n, err := h.store.GetNode(id)
	if err != nil {
		slog.ErrorContext(r.Context(), "load node for delete", "error", err, "node_id", id)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to load node")
		return
	}
	if n == nil {
		proxy.WriteError(w, r, http.StatusNotFound, "not_found", "invalid_request_error", "Node not found")
		return
	}
	if n.Source == "config" {
		h.configSourcedNodeError(w, r)
		return
	}

	if err := h.store.DisableNode(id); err != nil {
		if errors.Is(err, store.ErrNodeNotFound) {
			proxy.WriteError(w, r, http.StatusNotFound, "not_found", "invalid_request_error", "Node not found")
			return
		}
		slog.ErrorContext(r.Context(), "disable node error", "error", err, "node_id", id)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to disable node")
		return
	}

	// The refresher's reload reconciles the registry to the DB, removing the
	// now-disabled node from routing before we answer. A failure here is
	// surfaced: "deleted" must mean "no longer routable", not "will stop
	// being routable within a poll interval".
	if err := h.runNodeRefresh(r, id); err != nil {
		slog.ErrorContext(r.Context(), "node refresh after delete", "error", err, "node_id", id)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Node disabled but not yet removed from routing")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// refreshNode handles POST /api/admin/nodes/{id}/refresh: force an immediate
// probe + rediscovery and return the node with its fresh live state.
func (h *handler) refreshNode(w http.ResponseWriter, r *http.Request) {
	id, ok := h.nodeIDFromPath(w, r)
	if !ok {
		return
	}

	n, err := h.store.GetNode(id)
	if err != nil {
		slog.ErrorContext(r.Context(), "load node for refresh", "error", err, "node_id", id)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to load node")
		return
	}
	if n == nil {
		proxy.WriteError(w, r, http.StatusNotFound, "not_found", "invalid_request_error", "Node not found")
		return
	}

	if err := h.runNodeRefresh(r, id); err != nil {
		slog.ErrorContext(r.Context(), "node refresh error", "error", err, "node_id", id)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to refresh node")
		return
	}
	h.writeNodeResponse(w, r, id, http.StatusOK)
}

// nodeIDFromPath parses the {id} path segment, writing a 400 on garbage.
func (h *handler) nodeIDFromPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_id", "invalid_request_error", "Invalid node ID")
		return 0, false
	}
	return id, true
}
