package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestSetup_DefaultLevel(t *testing.T) {
	logger, err := Setup("info")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestSetup_AllLevels(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error", "DEBUG", "INFO", "WARN", "ERROR"} {
		logger, err := Setup(level)
		if err != nil {
			t.Errorf("Setup(%q): unexpected error: %v", level, err)
		}
		if logger == nil {
			t.Errorf("Setup(%q): expected non-nil logger", level)
		}
	}
}

func TestSetup_InvalidLevel(t *testing.T) {
	_, err := Setup("trace")
	if err == nil {
		t.Fatal("expected error for invalid level")
	}
}

func TestContextHandler_InjectsRequestID(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(&ContextHandler{Inner: inner})

	ctx := WithRequestID(context.Background(), "req_abc123")
	logger.InfoContext(ctx, "test message")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal log entry: %v", err)
	}

	if entry["request_id"] != "req_abc123" {
		t.Errorf("expected request_id 'req_abc123', got %v", entry["request_id"])
	}
	if entry["msg"] != "test message" {
		t.Errorf("expected msg 'test message', got %v", entry["msg"])
	}
}

func TestContextHandler_NoRequestID(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(&ContextHandler{Inner: inner})

	logger.InfoContext(context.Background(), "no id")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal log entry: %v", err)
	}

	if _, exists := entry["request_id"]; exists {
		t.Error("expected no request_id field when context has none")
	}
}

func TestContextHandler_JSONOutput(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(&ContextHandler{Inner: inner})

	logger.Info("startup", "port", "8080")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("expected valid JSON output, got: %s", buf.String())
	}
	if entry["level"] != "INFO" {
		t.Errorf("expected level INFO, got %v", entry["level"])
	}
	if entry["port"] != "8080" {
		t.Errorf("expected port '8080', got %v", entry["port"])
	}
}

func TestWithRequestID_Roundtrip(t *testing.T) {
	ctx := WithRequestID(context.Background(), "req_test123")
	got := RequestIDFromContext(ctx)
	if got != "req_test123" {
		t.Errorf("expected 'req_test123', got %q", got)
	}
}

func TestRequestIDFromContext_Empty(t *testing.T) {
	got := RequestIDFromContext(context.Background())
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestContextHandler_WithAttrsAndWithGroup(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	ch := &ContextHandler{Inner: inner}

	withAttrs := ch.WithAttrs([]slog.Attr{slog.String("component", "api")})
	if _, ok := withAttrs.(*ContextHandler); !ok {
		t.Fatal("WithAttrs should return a *ContextHandler")
	}

	withGroup := ch.WithGroup("request")
	if _, ok := withGroup.(*ContextHandler); !ok {
		t.Fatal("WithGroup should return a *ContextHandler")
	}

	// Exercise the combined handler end-to-end to make sure records flow.
	logger := slog.New(withAttrs)
	logger.InfoContext(context.Background(), "hello")
	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["component"] != "api" {
		t.Errorf("expected component=api attribute to flow through, got %+v", out)
	}
}
