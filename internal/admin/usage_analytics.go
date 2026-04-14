package admin

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/krishna/local-ai-proxy/internal/proxy"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// --- Response DTOs ---------------------------------------------------------
//
// These are defined separately from the store types so the frontend contract
// is snake_case and independent of internal field names.

type usageSummaryDTO struct {
	Requests         int     `json:"requests"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Credits          float64 `json:"credits"`
	AvgDurationMs    float64 `json:"avg_duration_ms"`
	Errors           int     `json:"errors"`
}

type modelUsageDTO struct {
	Model         string  `json:"model"`
	Requests      int     `json:"requests"`
	TotalTokens   int     `json:"total_tokens"`
	Credits       float64 `json:"credits"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
}

type ownerUsageDTO struct {
	OwnerType   string  `json:"owner_type"`
	UserID      *int64  `json:"user_id"`
	Email       *string `json:"email"`
	Name        *string `json:"name"`
	AccountID   *int64  `json:"account_id"`
	AccountName *string `json:"account_name"`
	AccountType *string `json:"account_type"`
	Requests    int     `json:"requests"`
	TotalTokens int     `json:"total_tokens"`
	Credits     float64 `json:"credits"`
	KeyCount    int     `json:"key_count"`
}

type timeseriesBucketDTO struct {
	Bucket           time.Time `json:"bucket"`
	Requests         int       `json:"requests"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	Credits          float64   `json:"credits"`
	Errors           int       `json:"errors"`
}

// --- Query parsing ---------------------------------------------------------

const (
	defaultLookback       = 7 * 24 * time.Hour
	defaultPaginationSize = 100
	maxPaginationSize     = 500
	// intervalHourCutoff is the window size above which the default interval
	// for the timeseries endpoint flips from "hour" to "day". Callers can
	// always override via ?interval=.
	intervalHourCutoff = 48 * time.Hour
)

// parseTimeParam accepts RFC3339 or YYYY-MM-DD. Returns a descriptive error
// with a short code suitable for proxy.WriteError.
func parseTimeParam(raw string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	t, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// parseInt64QueryParam parses a non-empty positive int64. Empty returns nil.
// Zero, negative, or non-numeric returns an error.
func parseInt64QueryParam(raw string) (*int64, error) {
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, err
	}
	if n <= 0 {
		return nil, strconv.ErrRange
	}
	return &n, nil
}

// parseUsageFilter extracts since/until/account_id/api_key_id/user_id/model
// from the request query string. Missing since/until are defaulted to a
// rolling 7-day window ending at "now". Returns a 4xx-ready error if any
// value is malformed; the code returned is the short "invalid_*" code the
// handler should pass into proxy.WriteError.
func parseUsageFilter(r *http.Request, now time.Time) (store.UsageFilter, string, string, error) {
	var f store.UsageFilter
	q := r.URL.Query()

	// Time window — since/until optional; default to [now-7d, now).
	if raw := q.Get("since"); raw != "" {
		t, err := parseTimeParam(raw)
		if err != nil {
			return f, "invalid_since", "Invalid since parameter (use RFC3339 or YYYY-MM-DD)", err
		}
		f.Since = &t
	} else {
		t := now.Add(-defaultLookback)
		f.Since = &t
	}
	if raw := q.Get("until"); raw != "" {
		t, err := parseTimeParam(raw)
		if err != nil {
			return f, "invalid_until", "Invalid until parameter (use RFC3339 or YYYY-MM-DD)", err
		}
		f.Until = &t
	} else {
		t := now
		f.Until = &t
	}
	if !f.Since.Before(*f.Until) {
		return f, "invalid_time_range", "since must be strictly before until", errInvalidTimeRange
	}

	// Optional positive int64 filters.
	if id, err := parseInt64QueryParam(q.Get("account_id")); err != nil {
		return f, "invalid_account_id", "Invalid account_id parameter (must be positive integer)", err
	} else {
		f.AccountID = id
	}
	if id, err := parseInt64QueryParam(q.Get("api_key_id")); err != nil {
		return f, "invalid_api_key_id", "Invalid api_key_id parameter (must be positive integer)", err
	} else {
		f.APIKeyID = id
	}
	if id, err := parseInt64QueryParam(q.Get("user_id")); err != nil {
		return f, "invalid_user_id", "Invalid user_id parameter (must be positive integer)", err
	} else {
		f.UserID = id
	}
	if m := q.Get("model"); m != "" {
		f.Model = &m
	}
	return f, "", "", nil
}

// errInvalidTimeRange is returned when since >= until after defaulting.
var errInvalidTimeRange = &usageParseError{msg: "since must be strictly before until"}

type usageParseError struct{ msg string }

func (e *usageParseError) Error() string { return e.msg }

// parsePagination reads ?limit=&offset= with defaults and caps. Returns
// (limit, offset, code, message, err). Empty code signals success.
func parsePagination(r *http.Request) (int, int, string, string, error) {
	q := r.URL.Query()
	limit := defaultPaginationSize
	offset := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return 0, 0, "invalid_limit", "Invalid limit parameter", err
		}
		if n <= 0 {
			return 0, 0, "invalid_limit", "limit must be a positive integer", strconv.ErrRange
		}
		if n > maxPaginationSize {
			return 0, 0, "invalid_limit", "limit exceeds maximum of 500", strconv.ErrRange
		}
		limit = n
	}
	if raw := q.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return 0, 0, "invalid_offset", "Invalid offset parameter", err
		}
		if n < 0 {
			return 0, 0, "invalid_offset", "offset must be non-negative", strconv.ErrRange
		}
		offset = n
	}
	return limit, offset, "", "", nil
}

// parseInterval returns "hour" or "day". If absent, defaults to "hour" for
// windows <=48h, "day" otherwise.
func parseInterval(r *http.Request, since, until time.Time) (string, string, string, error) {
	raw := r.URL.Query().Get("interval")
	if raw == "" {
		if until.Sub(since) <= intervalHourCutoff {
			return "hour", "", "", nil
		}
		return "day", "", "", nil
	}
	if raw != "hour" && raw != "day" {
		return "", "invalid_interval", "interval must be 'hour' or 'day'", errInvalidInterval
	}
	return raw, "", "", nil
}

var errInvalidInterval = &usageParseError{msg: "invalid interval"}

// --- Handlers --------------------------------------------------------------

func (h *handler) getUsageSummary(w http.ResponseWriter, r *http.Request) {
	f, code, msg, err := parseUsageFilter(r, time.Now())
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, code, "invalid_request_error", msg)
		return
	}

	summary, err := h.store.GetUsageSummary(f)
	if err != nil {
		slog.ErrorContext(r.Context(), "usage summary error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to get usage summary")
		return
	}

	writeEnvelope(w, usageSummaryDTO{
		Requests:         summary.Requests,
		PromptTokens:     summary.PromptTokens,
		CompletionTokens: summary.CompletionTokens,
		TotalTokens:      summary.TotalTokens,
		Credits:          summary.Credits,
		AvgDurationMs:    summary.AvgDurationMs,
		Errors:           summary.Errors,
	}, nil)
}

func (h *handler) getUsageByModel(w http.ResponseWriter, r *http.Request) {
	f, code, msg, err := parseUsageFilter(r, time.Now())
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, code, "invalid_request_error", msg)
		return
	}
	limit, offset, pcode, pmsg, perr := parsePagination(r)
	if perr != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, pcode, "invalid_request_error", pmsg)
		return
	}

	rows, err := h.store.GetUsageByModel(f)
	if err != nil {
		slog.ErrorContext(r.Context(), "usage by model error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to get usage by model")
		return
	}

	dtos := make([]modelUsageDTO, len(rows))
	for i, r := range rows {
		dtos[i] = modelUsageDTO{
			Model:         r.Model,
			Requests:      r.Requests,
			TotalTokens:   r.TotalTokens,
			Credits:       r.Credits,
			AvgDurationMs: r.AvgDurationMs,
		}
	}
	page, pag := sliceWindow(dtos, limit, offset)
	writeEnvelope(w, page, pag)
}

func (h *handler) getUsageByUser(w http.ResponseWriter, r *http.Request) {
	f, code, msg, err := parseUsageFilter(r, time.Now())
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, code, "invalid_request_error", msg)
		return
	}
	limit, offset, pcode, pmsg, perr := parsePagination(r)
	if perr != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, pcode, "invalid_request_error", pmsg)
		return
	}

	rows, err := h.store.GetUsageByUser(f)
	if err != nil {
		slog.ErrorContext(r.Context(), "usage by user error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to get usage by user")
		return
	}

	dtos := make([]ownerUsageDTO, len(rows))
	for i, row := range rows {
		dtos[i] = ownerUsageDTO{
			OwnerType:   deriveOwnerType(row),
			UserID:      row.UserID,
			Email:       row.Email,
			Name:        row.Name,
			AccountID:   row.AccountID,
			AccountName: row.AccountName,
			AccountType: row.AccountType,
			Requests:    row.Requests,
			TotalTokens: row.TotalTokens,
			Credits:     row.Credits,
			KeyCount:    row.KeyCount,
		}
	}
	page, pag := sliceWindow(dtos, limit, offset)
	writeEnvelope(w, page, pag)
}

func (h *handler) getUsageTimeseries(w http.ResponseWriter, r *http.Request) {
	f, code, msg, err := parseUsageFilter(r, time.Now())
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, code, "invalid_request_error", msg)
		return
	}
	interval, icode, imsg, ierr := parseInterval(r, *f.Since, *f.Until)
	if ierr != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, icode, "invalid_request_error", imsg)
		return
	}

	rows, err := h.store.GetUsageTimeseries(f, interval)
	if err != nil {
		slog.ErrorContext(r.Context(), "usage timeseries error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to get usage timeseries")
		return
	}

	filled := gapFillBuckets(rows, *f.Since, *f.Until, interval)
	dtos := make([]timeseriesBucketDTO, len(filled))
	for i, b := range filled {
		dtos[i] = timeseriesBucketDTO{
			Bucket:           b.Bucket,
			Requests:         b.Requests,
			PromptTokens:     b.PromptTokens,
			CompletionTokens: b.CompletionTokens,
			TotalTokens:      b.TotalTokens,
			Credits:          b.Credits,
			Errors:           b.Errors,
		}
	}
	writeEnvelope(w, dtos, nil)
}

// deriveOwnerType maps the three populations documented in PLAN.md §By-User.
func deriveOwnerType(r store.OwnerUsageRow) string {
	switch {
	case r.UserID != nil:
		return "user"
	case r.AccountID != nil:
		return "service"
	default:
		return "unattributed"
	}
}

// truncateToInterval normalizes t to UTC and truncates it to the start of the
// given interval so gap-fill bucket boundaries line up with date_trunc output.
func truncateToInterval(t time.Time, interval string) time.Time {
	u := t.UTC()
	switch interval {
	case "day":
		return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	default: // hour
		return time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), 0, 0, 0, time.UTC)
	}
}

func advanceBucket(t time.Time, interval string) time.Time {
	if interval == "day" {
		return t.AddDate(0, 0, 1)
	}
	return t.Add(time.Hour)
}

// gapFillBuckets returns a dense series for [since, until) by emitting a
// zero-valued bucket for any interval step missing from rows. Rows are
// assumed to be sorted ascending and their Bucket values to already be
// aligned to interval boundaries (date_trunc output).
func gapFillBuckets(rows []store.TimeseriesBucket, since, until time.Time, interval string) []store.TimeseriesBucket {
	start := truncateToInterval(since, interval)
	end := until.UTC()

	byBucket := make(map[time.Time]store.TimeseriesBucket, len(rows))
	for _, r := range rows {
		byBucket[r.Bucket.UTC()] = r
	}

	out := make([]store.TimeseriesBucket, 0, len(rows))
	for b := start; b.Before(end); b = advanceBucket(b, interval) {
		if row, ok := byBucket[b]; ok {
			row.Bucket = b
			out = append(out, row)
		} else {
			out = append(out, store.TimeseriesBucket{Bucket: b})
		}
	}
	return out
}
