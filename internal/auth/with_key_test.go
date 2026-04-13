package auth

import (
	"context"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/store"
)

func TestWithKeyAndKeyFromContext(t *testing.T) {
	k := &store.APIKey{ID: 7, Name: "wk"}
	ctx := WithKey(context.Background(), k)
	got := KeyFromContext(ctx)
	if got == nil || got.ID != 7 {
		t.Errorf("WithKey roundtrip failed: got %+v", got)
	}

	if KeyFromContext(context.Background()) != nil {
		t.Error("expected nil from empty context")
	}
}
