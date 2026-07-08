package apierror

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DecodeJSON decodes the request body into dst and requires it to be a
// single JSON document. On failure it writes the standard error envelope —
// 413 when the body exceeded a MaxBytesReader cap (see middleware.MaxBody),
// 400 for anything else — and returns false, in which case the handler must
// return without writing a response.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeJSON(w, r, dst, false)
}

// DecodeJSONStrict behaves like DecodeJSON but additionally rejects unknown
// fields with a 400 that names the offending field. Use it on endpoints
// where a silently dropped field would change semantics — e.g. the pricing
// endpoint after the per-token → per-MTok rate re-denomination, where the
// old field names must fail loudly instead of being ignored.
func DecodeJSONStrict(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeJSON(w, r, dst, true)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, strict bool) bool {
	dec := json.NewDecoder(r.Body)
	if strict {
		dec.DisallowUnknownFields()
	}
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
	if field, ok := unknownField(err); ok {
		WriteError(w, r, http.StatusBadRequest, "unknown_field", "invalid_request_error",
			fmt.Sprintf("Unknown field %s", field))
		return
	}
	WriteError(w, r, http.StatusBadRequest, "invalid_json", "invalid_request_error", "Invalid JSON body")
}

// unknownField extracts the field name (with quotes) from encoding/json's
// unknown-field error. The stdlib exposes no typed error for it, so match
// the stable message prefix.
func unknownField(err error) (string, bool) {
	const prefix = `json: unknown field `
	if msg := err.Error(); strings.HasPrefix(msg, prefix) {
		return strings.TrimPrefix(msg, prefix), true
	}
	return "", false
}
