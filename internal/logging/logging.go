package logging

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

type requestIDKey struct{}

// WithRequestID stores a request ID in the context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFromContext retrieves the request ID from the context, or "".
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// ContextHandler wraps an slog.Handler and automatically injects
// request_id from the context into every log record.
type ContextHandler struct {
	Inner slog.Handler
}

func (h *ContextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.Inner.Enabled(ctx, level)
}

func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestIDFromContext(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Inner.Handle(ctx, r)
}

func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{Inner: h.Inner.WithAttrs(attrs)}
}

func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{Inner: h.Inner.WithGroup(name)}
}

// Setup creates a JSON slog.Logger at the given level, wrapped with
// ContextHandler for automatic request_id injection.
// Valid levels: debug, info, warn, error (case-insensitive).
func Setup(level string) (*slog.Logger, error) {
	var slogLevel slog.Level
	switch strings.ToLower(level) {
	case "debug":
		slogLevel = slog.LevelDebug
	case "info":
		slogLevel = slog.LevelInfo
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid log level %q: must be debug, info, warn, or error", level)
	}

	inner := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slogLevel,
	})
	logger := slog.New(&ContextHandler{Inner: inner})
	return logger, nil
}
