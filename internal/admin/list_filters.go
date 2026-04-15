package admin

import (
	"net/http"
	"strconv"
)

// wantEnvelope reports whether the response should use the `{data, pagination}`
// envelope. As of BE 7 envelope is the default; callers can opt out with
// `?envelope=0` for one deprecation release as a safety valve for ad-hoc
// `X-Admin-Key` scripts. Any other non-empty value is a validation error so
// typos like `envelope=true` don't silently fall through.
func wantEnvelope(r *http.Request) (bool, string, string, error) {
	raw := r.URL.Query().Get("envelope")
	switch raw {
	case "", "1":
		return true, "", "", nil
	case "0":
		return false, "", "", nil
	default:
		return false, "invalid_envelope", "envelope must be 0 or 1", strconv.ErrSyntax
	}
}

// parseIsActiveFilter parses `?is_active=true|false`. Empty string returns
// nil (no filter). Any other value is a validation error — no "1"/"0"/"yes"
// aliases so the contract stays tight.
func parseIsActiveFilter(r *http.Request) (*bool, string, string, error) {
	raw := r.URL.Query().Get("is_active")
	if raw == "" {
		return nil, "", "", nil
	}
	switch raw {
	case "true":
		v := true
		return &v, "", "", nil
	case "false":
		v := false
		return &v, "", "", nil
	default:
		return nil, "invalid_is_active", "is_active must be true or false", strconv.ErrSyntax
	}
}

// parseRoleFilter parses `?role=admin|user`. Empty string returns "" (no
// filter). Any other value is a validation error.
func parseRoleFilter(r *http.Request) (string, string, string, error) {
	raw := r.URL.Query().Get("role")
	switch raw {
	case "":
		return "", "", "", nil
	case "admin", "user":
		return raw, "", "", nil
	default:
		return "", "invalid_role", "role must be 'admin' or 'user'", strconv.ErrSyntax
	}
}

// parseAccountTypeFilter parses `?type=personal|service`. Empty returns "".
func parseAccountTypeFilter(r *http.Request) (string, string, string, error) {
	raw := r.URL.Query().Get("type")
	switch raw {
	case "":
		return "", "", "", nil
	case "personal", "service":
		return raw, "", "", nil
	default:
		return "", "invalid_type", "type must be 'personal' or 'service'", strconv.ErrSyntax
	}
}
