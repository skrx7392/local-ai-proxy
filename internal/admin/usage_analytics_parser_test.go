package admin

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/store"
)

var parserNow = time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)

// ---- parseUsageFilter ----

func TestParseUsageFilter_DefaultsTo7DayWindow(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/admin/usage/summary", nil)
	f, code, _, err := parseUsageFilter(req, parserNow)
	if err != nil {
		t.Fatalf("unexpected error: %v (code=%s)", err, code)
	}
	if f.Since == nil || f.Until == nil {
		t.Fatal("since/until should be defaulted")
	}
	if !f.Until.Equal(parserNow) {
		t.Errorf("until = %v, want %v", f.Until, parserNow)
	}
	wantSince := parserNow.Add(-defaultLookback)
	if !f.Since.Equal(wantSince) {
		t.Errorf("since = %v, want %v", f.Since, wantSince)
	}
}

func TestParseUsageFilter_RFC3339AndDateOnly(t *testing.T) {
	cases := []struct {
		name, qs string
		wantErr  bool
	}{
		{"rfc3339", "since=2026-04-01T00:00:00Z&until=2026-04-10T00:00:00Z", false},
		{"date-only", "since=2026-04-01&until=2026-04-10", false},
		{"bad since", "since=nope", true},
		{"bad until", "since=2026-04-01&until=blah", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/?"+c.qs, nil)
			_, _, _, err := parseUsageFilter(req, parserNow)
			if (err != nil) != c.wantErr {
				t.Errorf("err=%v wantErr=%v", err, c.wantErr)
			}
		})
	}
}

func TestParseUsageFilter_SinceMustBeBeforeUntil(t *testing.T) {
	// Equal boundaries are rejected (strict <); otherwise a timeseries would
	// produce zero buckets even though the user submitted a valid pair of
	// dates.
	req := httptest.NewRequest("GET", "/?since=2026-04-10&until=2026-04-10", nil)
	_, code, _, err := parseUsageFilter(req, parserNow)
	if err == nil {
		t.Fatal("expected error for since == until")
	}
	if code != "invalid_time_range" {
		t.Errorf("code = %q, want invalid_time_range", code)
	}

	req2 := httptest.NewRequest("GET", "/?since=2026-04-11&until=2026-04-10", nil)
	_, code2, _, err2 := parseUsageFilter(req2, parserNow)
	if err2 == nil || code2 != "invalid_time_range" {
		t.Errorf("reversed range: err=%v code=%q", err2, code2)
	}
}

func TestParseUsageFilter_RejectsNonPositiveIDs(t *testing.T) {
	cases := []struct {
		name, qs, wantCode string
	}{
		{"zero account_id", "account_id=0", "invalid_account_id"},
		{"negative account_id", "account_id=-1", "invalid_account_id"},
		{"non-numeric api_key_id", "api_key_id=abc", "invalid_api_key_id"},
		{"zero user_id", "user_id=0", "invalid_user_id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/?"+c.qs, nil)
			_, code, _, err := parseUsageFilter(req, parserNow)
			if err == nil {
				t.Fatal("expected error")
			}
			if code != c.wantCode {
				t.Errorf("code = %q, want %q", code, c.wantCode)
			}
		})
	}
}

func TestParseUsageFilter_AcceptsValidPositiveIDs(t *testing.T) {
	req := httptest.NewRequest("GET", "/?account_id=5&api_key_id=7&user_id=9&model=llama", nil)
	f, _, _, err := parseUsageFilter(req, parserNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.AccountID == nil || *f.AccountID != 5 {
		t.Errorf("account_id = %v, want 5", f.AccountID)
	}
	if f.APIKeyID == nil || *f.APIKeyID != 7 {
		t.Errorf("api_key_id = %v, want 7", f.APIKeyID)
	}
	if f.UserID == nil || *f.UserID != 9 {
		t.Errorf("user_id = %v, want 9", f.UserID)
	}
	if f.Model == nil || *f.Model != "llama" {
		t.Errorf("model = %v, want llama", f.Model)
	}
}

// ---- parsePagination ----

func TestParsePagination_Defaults(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	limit, offset, _, _, err := parsePagination(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if limit != defaultPaginationSize {
		t.Errorf("limit = %d, want %d", limit, defaultPaginationSize)
	}
	if offset != 0 {
		t.Errorf("offset = %d, want 0", offset)
	}
}

func TestParsePagination_Caps(t *testing.T) {
	cases := []struct {
		name, qs, wantCode string
	}{
		{"limit zero", "limit=0", "invalid_limit"},
		{"limit negative", "limit=-1", "invalid_limit"},
		{"limit over cap", "limit=501", "invalid_limit"},
		{"limit non-numeric", "limit=abc", "invalid_limit"},
		{"offset negative", "offset=-5", "invalid_offset"},
		{"offset non-numeric", "offset=x", "invalid_offset"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/?"+c.qs, nil)
			_, _, code, _, err := parsePagination(req)
			if err == nil {
				t.Fatal("expected error")
			}
			if code != c.wantCode {
				t.Errorf("code = %q, want %q", code, c.wantCode)
			}
		})
	}
}

// ---- parseInterval ----

func TestParseInterval_DefaultDependsOnWindow(t *testing.T) {
	t0 := parserNow
	cases := []struct {
		name       string
		since      time.Time
		until      time.Time
		wantResult string
	}{
		{"48h exact → hour", t0.Add(-48 * time.Hour), t0, "hour"},
		{"49h → day", t0.Add(-49 * time.Hour), t0, "day"},
		{"1h → hour", t0.Add(-time.Hour), t0, "hour"},
		{"30d → day", t0.Add(-30 * 24 * time.Hour), t0, "day"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			got, _, _, err := parseInterval(req, c.since, c.until)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.wantResult {
				t.Errorf("interval = %q, want %q", got, c.wantResult)
			}
		})
	}
}

func TestParseInterval_ExplicitValues(t *testing.T) {
	req := httptest.NewRequest("GET", "/?interval=day", nil)
	got, _, _, err := parseInterval(req, parserNow.Add(-time.Hour), parserNow)
	if err != nil || got != "day" {
		t.Errorf("got %q err %v, want day nil", got, err)
	}

	req2 := httptest.NewRequest("GET", "/?interval=hour", nil)
	got2, _, _, err2 := parseInterval(req2, parserNow.Add(-30*24*time.Hour), parserNow)
	if err2 != nil || got2 != "hour" {
		t.Errorf("got %q err %v, want hour nil", got2, err2)
	}
}

func TestParseInterval_InvalidRejected(t *testing.T) {
	req := httptest.NewRequest("GET", "/?interval=week", nil)
	_, code, _, err := parseInterval(req, parserNow.Add(-time.Hour), parserNow)
	if err == nil {
		t.Fatal("expected error")
	}
	if code != "invalid_interval" {
		t.Errorf("code = %q, want invalid_interval", code)
	}
}

// ---- deriveOwnerType ----

func TestDeriveOwnerType(t *testing.T) {
	u := int64(1)
	a := int64(2)
	if got := deriveOwnerType(store.OwnerUsageRow{UserID: &u, AccountID: &a}); got != "user" {
		t.Errorf("user+account should be user, got %q", got)
	}
	if got := deriveOwnerType(store.OwnerUsageRow{UserID: &u}); got != "user" {
		t.Errorf("user only should be user, got %q", got)
	}
	if got := deriveOwnerType(store.OwnerUsageRow{AccountID: &a}); got != "service" {
		t.Errorf("account only should be service, got %q", got)
	}
	if got := deriveOwnerType(store.OwnerUsageRow{}); got != "unattributed" {
		t.Errorf("no ids should be unattributed, got %q", got)
	}
}

// ---- gapFillBuckets ----

func TestGapFillBuckets_FillsMiddleHole(t *testing.T) {
	since := time.Date(2026, 4, 14, 10, 0, 0, 0, time.UTC)
	until := time.Date(2026, 4, 14, 13, 0, 0, 0, time.UTC)
	// Rows at 10:00 and 12:00 — 11:00 must be zero-filled.
	rows := []store.TimeseriesBucket{
		{Bucket: since, Requests: 5},
		{Bucket: since.Add(2 * time.Hour), Requests: 7},
	}

	out := gapFillBuckets(rows, since, until, "hour")
	if len(out) != 3 {
		t.Fatalf("expected 3 buckets, got %d", len(out))
	}
	wantBuckets := []time.Time{since, since.Add(time.Hour), since.Add(2 * time.Hour)}
	for i, b := range out {
		if !b.Bucket.Equal(wantBuckets[i]) {
			t.Errorf("bucket[%d] = %v, want %v", i, b.Bucket, wantBuckets[i])
		}
	}
	if out[0].Requests != 5 || out[1].Requests != 0 || out[2].Requests != 7 {
		t.Errorf("requests = %d/%d/%d, want 5/0/7",
			out[0].Requests, out[1].Requests, out[2].Requests)
	}
}

func TestGapFillBuckets_ExclusiveUntil(t *testing.T) {
	// An until at exactly 13:00 must not emit a 13:00 bucket.
	since := time.Date(2026, 4, 14, 10, 0, 0, 0, time.UTC)
	until := time.Date(2026, 4, 14, 13, 0, 0, 0, time.UTC)
	out := gapFillBuckets(nil, since, until, "hour")
	if len(out) != 3 {
		t.Fatalf("expected 3 hourly buckets in [10, 13), got %d", len(out))
	}
	last := out[len(out)-1].Bucket
	if !last.Equal(since.Add(2 * time.Hour)) {
		t.Errorf("last bucket = %v, want 12:00", last)
	}
}

func TestGapFillBuckets_TruncatesUnalignedSince(t *testing.T) {
	// since=10:37 should truncate to 10:00 so buckets line up with SQL
	// date_trunc output.
	since := time.Date(2026, 4, 14, 10, 37, 0, 0, time.UTC)
	until := time.Date(2026, 4, 14, 13, 0, 0, 0, time.UTC)
	out := gapFillBuckets(nil, since, until, "hour")
	if len(out) != 3 {
		t.Fatalf("expected 3 buckets, got %d", len(out))
	}
	if !out[0].Bucket.Equal(time.Date(2026, 4, 14, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("first bucket = %v, want 10:00 UTC", out[0].Bucket)
	}
}

func TestGapFillBuckets_DayInterval(t *testing.T) {
	since := time.Date(2026, 4, 14, 6, 0, 0, 0, time.UTC)
	until := time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC)
	out := gapFillBuckets(nil, since, until, "day")
	if len(out) != 3 {
		t.Fatalf("expected 3 daily buckets, got %d", len(out))
	}
	// Each bucket must be at 00:00 UTC of successive days.
	for i, b := range out {
		want := time.Date(2026, 4, 14+i, 0, 0, 0, 0, time.UTC)
		if !b.Bucket.Equal(want) {
			t.Errorf("bucket[%d] = %v, want %v", i, b.Bucket, want)
		}
	}
}
