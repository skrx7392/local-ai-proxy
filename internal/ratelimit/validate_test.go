package ratelimit

import (
	"errors"
	"testing"
)

func TestApplyConfigDefaultsAndCap(t *testing.T) {
	cases := []struct {
		name    string
		input   int
		want    int
		wantErr error
	}{
		{name: "zero maps to default 60", input: 0, want: 60},
		{name: "negative maps to default 60", input: -5, want: 60},
		{name: "explicit 60 is preserved", input: 60, want: 60},
		{name: "explicit 1 is preserved", input: 1, want: 1},
		{name: "explicit 10000 is preserved", input: 10000, want: 10000},
		{name: "10001 is rejected", input: 10001, wantErr: ErrConfigTooHigh},
		{name: "huge value is rejected", input: 1_000_000, wantErr: ErrConfigTooHigh},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ApplyConfigDefaultsAndCap(tc.input)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("expected error %v, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}
