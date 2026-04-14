package admin

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestEnvelope_WithPagination(t *testing.T) {
	rec := httptest.NewRecorder()
	writeEnvelope(rec, []int{1, 2, 3}, &Pagination{Limit: 10, Offset: 0, Total: 3})

	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var decoded struct {
		Data       []int       `json:"data"`
		Pagination *Pagination `json:"pagination"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Data) != 3 {
		t.Errorf("data length = %d, want 3", len(decoded.Data))
	}
	if decoded.Pagination == nil {
		t.Fatal("pagination should be present")
	}
	if decoded.Pagination.Total != 3 {
		t.Errorf("pagination.total = %d, want 3", decoded.Pagination.Total)
	}
}

func TestEnvelope_OmitsPaginationWhenNil(t *testing.T) {
	rec := httptest.NewRecorder()
	writeEnvelope(rec, map[string]int{"requests": 42}, nil)

	// Raw body must not contain the pagination key so the contract with the
	// frontend is unambiguous for single-object endpoints.
	if got := rec.Body.String(); contains(got, `"pagination"`) {
		t.Errorf("expected no pagination key in response, got %s", got)
	}

	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["pagination"]; ok {
		t.Error("pagination key must be omitted when nil")
	}
	if _, ok := decoded["data"]; !ok {
		t.Error("data key must always be present")
	}
}

func TestSliceWindow_Basic(t *testing.T) {
	items := []int{10, 20, 30, 40, 50}
	got, pag := sliceWindow(items, 2, 1)
	want := []int{20, 30}
	if !equalInts(got, want) {
		t.Errorf("sliceWindow = %v, want %v", got, want)
	}
	if pag.Total != 5 || pag.Limit != 2 || pag.Offset != 1 {
		t.Errorf("pagination = %+v, want {limit:2, offset:1, total:5}", pag)
	}
}

func TestSliceWindow_OffsetBeyondEnd(t *testing.T) {
	items := []int{1, 2, 3}
	got, pag := sliceWindow(items, 10, 100)
	if len(got) != 0 {
		t.Errorf("expected empty slice for offset beyond end, got %v", got)
	}
	// Total must still reflect the pre-slice length so the client can render
	// "showing 0–0 of 3" rather than assuming the list is empty.
	if pag.Total != 3 {
		t.Errorf("total = %d, want 3", pag.Total)
	}
}

func TestSliceWindow_LimitExceedsRemainder(t *testing.T) {
	items := []int{1, 2, 3, 4}
	got, pag := sliceWindow(items, 10, 2)
	want := []int{3, 4}
	if !equalInts(got, want) {
		t.Errorf("sliceWindow = %v, want %v", got, want)
	}
	if pag.Total != 4 {
		t.Errorf("total = %d, want 4", pag.Total)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
