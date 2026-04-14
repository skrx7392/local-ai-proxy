package ratelimit

import "errors"

// MaxConfigPerMinute is the upper bound accepted when creating or updating
// a key's per-minute rate limit. Values above this are rejected as clearly
// unintended (typo guard).
const MaxConfigPerMinute = 10000

// DefaultConfigPerMinute is the value substituted when a caller omits
// rate_limit in a JSON body (which decodes to 0) or passes a non-positive
// value. Preserving this behavior keeps existing clients working.
const DefaultConfigPerMinute = 60

// ErrConfigTooHigh is returned by ApplyConfigDefaultsAndCap when the caller
// passes a value above MaxConfigPerMinute.
var ErrConfigTooHigh = errors.New("rate_limit exceeds maximum")

// ApplyConfigDefaultsAndCap normalizes an incoming rate_limit value:
//   - n <= 0 returns DefaultConfigPerMinute (preserves "omitted or zero"
//     compatibility for existing clients)
//   - n > MaxConfigPerMinute returns ErrConfigTooHigh
//   - otherwise returns n unchanged
func ApplyConfigDefaultsAndCap(n int) (int, error) {
	if n <= 0 {
		return DefaultConfigPerMinute, nil
	}
	if n > MaxConfigPerMinute {
		return 0, ErrConfigTooHigh
	}
	return n, nil
}
