package apierror

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// DecodeJSON decodes the request body into dst and requires it to be a
// single JSON document. On failure it writes the standard error envelope —
// 413 when the body exceeded a MaxBytesReader cap (see middleware.MaxBody),
// 400 for anything else — and returns false, in which case the handler must
// return without writing a response.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		writeDecodeError(w, r, err)
		return false
	}

	// Require EOF after the first document. Without this, a small valid
	// value followed by padding would never read past the MaxBytesReader
	// cap (silently accepting an oversized request), and a second document
	// could ride along unnoticed.
	switch tailErr := dec.Decode(new(json.RawMessage)); {
	case errors.Is(tailErr, io.EOF):
		return true
	case tailErr == nil:
		WriteError(w, r, http.StatusBadRequest, "invalid_json", "invalid_request_error", "Request body must contain a single JSON document")
		return false
	default:
		writeDecodeError(w, r, tailErr)
		return false
	}
}

func writeDecodeError(w http.ResponseWriter, r *http.Request, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		WriteError(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "invalid_request_error", "Request body too large")
		return
	}
	WriteError(w, r, http.StatusBadRequest, "invalid_json", "invalid_request_error", "Invalid JSON body")
}
