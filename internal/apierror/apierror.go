package apierror

import (
	"encoding/json"
	"net/http"

	"github.com/krishna/local-ai-proxy/internal/logging"
)

// WriteError writes a JSON error response in the OpenAI-compatible format.
// If the request context contains a request ID, it is included as a
// top-level "request_id" field in the response body.
func WriteError(w http.ResponseWriter, r *http.Request, statusCode int, code, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	resp := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	}
	if r != nil {
		if id := logging.RequestIDFromContext(r.Context()); id != "" {
			resp["request_id"] = id
		}
	}
	json.NewEncoder(w).Encode(resp)
}
