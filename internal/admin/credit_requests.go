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
	"github.com/krishna/local-ai-proxy/internal/store"
)

// Credit-request admin surface (docs/design/credit-requests.md): list
// cap-hit requests and resolve them. Resolution never moves money — the
// caller (admin console, Discord bot) grants credits through
// POST /api/admin/accounts/{id}/credits first, then marks the request.

// parseCreditRequestStatus parses `?status=pending|granted|dismissed`.
// Empty defaults to pending — the actionable view.
func parseCreditRequestStatus(r *http.Request) (string, string, string, error) {
	raw := r.URL.Query().Get("status")
	switch raw {
	case "":
		return "pending", "", "", nil
	case "pending", "granted", "dismissed":
		return raw, "", "", nil
	default:
		return "", "invalid_status", "status must be 'pending', 'granted' or 'dismissed'", strconv.ErrSyntax
	}
}

type creditRequestResponse struct {
	ID           int64   `json:"id"`
	AccountID    int64   `json:"account_id"`
	AccountName  string  `json:"account_name"`
	Email        *string `json:"email"`
	Period       string  `json:"period"` // YYYY-MM-DD, first day of the month
	Status       string  `json:"status"`
	CreatedAt    string  `json:"created_at"`
	ResolvedAt   *string `json:"resolved_at"`
	ResolvedNote *string `json:"resolved_note"`
	// EffectiveMonthlyGrant resolves the per-account override against the
	// env default server-side so clients never hardcode the default.
	EffectiveMonthlyGrant float64 `json:"effective_monthly_grant"`
	Balance               float64 `json:"balance"`
}

func (h *handler) listCreditRequests(w http.ResponseWriter, r *http.Request) {
	envelope, ecode, emsg, eerr := wantEnvelope(r)
	if eerr != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, ecode, "invalid_request_error", emsg)
		return
	}
	status, scode, smsg, serr := parseCreditRequestStatus(r)
	if serr != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, scode, "invalid_request_error", smsg)
		return
	}
	limit, offset, pcode, pmsg, perr := parsePagination(r)
	if perr != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, pcode, "invalid_request_error", pmsg)
		return
	}

	rows, err := h.store.ListCreditRequests(status)
	if err != nil {
		slog.ErrorContext(r.Context(), "list credit requests error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to list credit requests")
		return
	}

	resp := make([]creditRequestResponse, 0, len(rows))
	for _, row := range rows {
		grant := h.endUserMonthlyGrant
		if row.MonthlyGrant != nil {
			grant = *row.MonthlyGrant
		}
		var resolvedAt *string
		if row.ResolvedAt != nil {
			v := row.ResolvedAt.Format(time.RFC3339)
			resolvedAt = &v
		}
		resp = append(resp, creditRequestResponse{
			ID:                    row.ID,
			AccountID:             row.AccountID,
			AccountName:           row.AccountName,
			Email:                 row.Email,
			Period:                row.Period.Format("2006-01-02"),
			Status:                row.Status,
			CreatedAt:             row.CreatedAt.Format(time.RFC3339),
			ResolvedAt:            resolvedAt,
			ResolvedNote:          row.ResolvedNote,
			EffectiveMonthlyGrant: grant,
			Balance:               row.Balance,
		})
	}

	if envelope {
		page, pag := sliceWindow(resp, limit, offset)
		writeEnvelope(w, page, pag)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *handler) resolveCreditRequest(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_id", "invalid_request_error", "Invalid credit request ID")
		return
	}

	var req struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if !apierror.DecodeJSON(w, r, &req) {
		return
	}
	if req.Status != "granted" && req.Status != "dismissed" {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_status", "invalid_request_error", "status must be 'granted' or 'dismissed'")
		return
	}

	if err := h.store.ResolveCreditRequest(id, req.Status, req.Note); err != nil {
		switch {
		case errors.Is(err, store.ErrCreditRequestNotFound):
			proxy.WriteError(w, r, http.StatusNotFound, "not_found", "invalid_request_error", "Credit request not found")
		case errors.Is(err, store.ErrCreditRequestResolved):
			proxy.WriteError(w, r, http.StatusConflict, "already_resolved", "invalid_request_error", "Credit request was already resolved")
		default:
			slog.ErrorContext(r.Context(), "resolve credit request error", "error", err, "credit_request_id", id)
			proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to resolve credit request")
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"id": id, "status": req.Status})
}
