package admin

import (
	"encoding/json"
	"net/http"
)

// Envelope is the standard wrapper for BE 2+ admin endpoints. Pagination is
// omitted from the response when nil (single-object responses such as the
// summary endpoint).
type Envelope struct {
	Data       any         `json:"data"`
	Pagination *Pagination `json:"pagination,omitempty"`
}

// Pagination carries cursor metadata for list responses. Total is the count
// of rows before slicing; Limit and Offset echo what the handler applied.
type Pagination struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
}

// writeEnvelope writes an Envelope{data, pagination} payload as JSON. Pass
// pagination=nil for single-object or non-paginated responses.
func writeEnvelope(w http.ResponseWriter, data any, pagination *Pagination) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Envelope{Data: data, Pagination: pagination})
}

// sliceWindow slices a list with the given limit/offset and returns the
// slice plus a Pagination describing the window. The total is always the
// pre-slice length so clients can render "showing X–Y of N".
func sliceWindow[T any](items []T, limit, offset int) ([]T, *Pagination) {
	total := len(items)
	if offset >= total {
		return []T{}, &Pagination{Limit: limit, Offset: offset, Total: total}
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return items[offset:end], &Pagination{Limit: limit, Offset: offset, Total: total}
}
