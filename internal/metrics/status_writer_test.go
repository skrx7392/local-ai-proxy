package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type flushableRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (f *flushableRecorder) Flush() { f.flushed = true }

func TestStatusWriter_WriteHeaderWriteFlushUnwrap(t *testing.T) {
	inner := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: inner}

	// First WriteHeader captures status, second is a no-op on status.
	sw.WriteHeader(http.StatusTeapot)
	sw.WriteHeader(http.StatusInternalServerError)
	if sw.status != http.StatusTeapot {
		t.Errorf("expected captured status 418, got %d", sw.status)
	}

	// Write without prior WriteHeader also sets wroteHeader internally.
	sw2 := &statusWriter{ResponseWriter: httptest.NewRecorder()}
	_, _ = sw2.Write([]byte("hi"))
	if !sw2.wroteHeader {
		t.Error("Write should mark wroteHeader")
	}

	// Unwrap returns the inner writer.
	if sw.Unwrap() != inner {
		t.Error("Unwrap should return inner ResponseWriter")
	}

	// Flush delegates when the inner writer implements Flusher.
	fr := &flushableRecorder{ResponseRecorder: httptest.NewRecorder()}
	sw3 := &statusWriter{ResponseWriter: fr}
	sw3.Flush()
	if !fr.flushed {
		t.Error("Flush should delegate to inner Flusher")
	}

	// Flush is a no-op when inner isn't a Flusher (struct{} wrapper) —
	// the call must not panic.
	sw4 := &statusWriter{ResponseWriter: nonFlushWriter{}}
	sw4.Flush()
}

// nonFlushWriter implements http.ResponseWriter but not http.Flusher.
type nonFlushWriter struct{}

func (nonFlushWriter) Header() http.Header       { return http.Header{} }
func (nonFlushWriter) Write([]byte) (int, error) { return 0, nil }
func (nonFlushWriter) WriteHeader(int)           {}
