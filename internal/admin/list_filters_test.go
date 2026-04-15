package admin

import (
	"net/http/httptest"
	"testing"
)

func TestWantEnvelope(t *testing.T) {
	cases := []struct {
		qs      string
		want    bool
		wantErr bool
		code    string
	}{
		{"", true, false, ""},
		{"envelope=0", false, false, ""},
		{"envelope=1", true, false, ""},
		{"envelope=true", false, true, "invalid_envelope"},
		{"envelope=yes", false, true, "invalid_envelope"},
		{"envelope=2", false, true, "invalid_envelope"},
	}
	for _, tc := range cases {
		t.Run(tc.qs, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/x?"+tc.qs, nil)
			got, code, _, err := wantEnvelope(req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.qs)
				}
				if code != tc.code {
					t.Errorf("code = %q, want %q", code, tc.code)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseIsActiveFilter(t *testing.T) {
	cases := []struct {
		qs      string
		wantPtr bool
		wantVal bool
		wantErr bool
	}{
		{"", false, false, false},
		{"is_active=true", true, true, false},
		{"is_active=false", true, false, false},
		{"is_active=1", false, false, true},
		{"is_active=yes", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.qs, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/x?"+tc.qs, nil)
			got, _, _, err := parseIsActiveFilter(req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.qs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantPtr {
				if got == nil {
					t.Fatal("expected non-nil pointer")
				}
				if *got != tc.wantVal {
					t.Errorf("*got = %v, want %v", *got, tc.wantVal)
				}
			} else if got != nil {
				t.Errorf("expected nil pointer, got %v", *got)
			}
		})
	}
}

func TestParseRoleFilter(t *testing.T) {
	for _, qs := range []string{"", "role=admin", "role=user"} {
		req := httptest.NewRequest("GET", "/x?"+qs, nil)
		if _, _, _, err := parseRoleFilter(req); err != nil {
			t.Errorf("unexpected err for %q: %v", qs, err)
		}
	}
	req := httptest.NewRequest("GET", "/x?role=superadmin", nil)
	if _, code, _, err := parseRoleFilter(req); err == nil || code != "invalid_role" {
		t.Errorf("expected invalid_role error, got code=%q err=%v", code, err)
	}
}

func TestParseAccountTypeFilter(t *testing.T) {
	for _, qs := range []string{"", "type=personal", "type=service"} {
		req := httptest.NewRequest("GET", "/x?"+qs, nil)
		if _, _, _, err := parseAccountTypeFilter(req); err != nil {
			t.Errorf("unexpected err for %q: %v", qs, err)
		}
	}
	req := httptest.NewRequest("GET", "/x?type=other", nil)
	if _, code, _, err := parseAccountTypeFilter(req); err == nil || code != "invalid_type" {
		t.Errorf("expected invalid_type error, got code=%q err=%v", code, err)
	}
}
