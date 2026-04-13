package user

import (
	"testing"
	"time"
)

func TestSessionDurationFor(t *testing.T) {
	cases := []struct {
		role string
		want time.Duration
	}{
		{role: "admin", want: 6 * time.Hour},
		{role: "user", want: 7 * 24 * time.Hour},
		{role: "", want: 7 * 24 * time.Hour},
		{role: "unknown", want: 7 * 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			got := SessionDurationFor(tc.role)
			if got != tc.want {
				t.Errorf("role=%q: got %s, want %s", tc.role, got, tc.want)
			}
		})
	}
}
