package apierror

import (
	"encoding/json"
	"errors"
	"net/http"
)

// DecodeJSON decodes the request body into dst. On failure it writes the
// standard error envelope — 413 when the body exceeded a MaxBytesReader cap
// (see middleware.MaxBody), 400 for anything else — and returns false, in
// which case the handler must return without writing a response.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	err := json.NewDecoder(r.Body).Decode(dst)
	if err == nil {
		return true
	}

	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		WriteError(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "invalid_request_error", "Request body too large")
		return false
	}
	WriteError(w, r, http.StatusBadRequest, "invalid_json", "invalid_request_error", "Invalid JSON body")
	return false
}
