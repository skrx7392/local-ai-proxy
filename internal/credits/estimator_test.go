package credits

import (
	"math"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/store"
)

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 0.000001
}

func TestEstimatePromptTokens(t *testing.T) {
	tests := []struct {
		bodyLen  int
		expected int
	}{
		{400, 100},
		{100, 25},
		{3, 1}, // minimum 1
		{0, 1}, // minimum 1
		{1000, 250},
	}
	for _, tt := range tests {
		got := EstimatePromptTokens(tt.bodyLen)
		if got != tt.expected {
			t.Errorf("EstimatePromptTokens(%d) = %d, want %d", tt.bodyLen, got, tt.expected)
		}
	}
}

func TestEstimateCompletionTokens_Cascade(t *testing.T) {
	maxTok := 200
	stats10 := &store.AccountUsageStats{AvgCompletionTokens: 150, RequestCount: 10}
	stats5 := &store.AccountUsageStats{AvgCompletionTokens: 150, RequestCount: 5}
	pricing := &store.CreditPricing{TypicalCompletion: 300}

	// 1. maxTokens takes priority
	got := EstimateCompletionTokens(&maxTok, stats10, pricing)
	if got != 200 {
		t.Errorf("with maxTokens: got %d, want 200", got)
	}

	// 2. Historical avg (10+ requests) when no maxTokens
	got = EstimateCompletionTokens(nil, stats10, pricing)
	if got != 150 {
		t.Errorf("with stats >= 10: got %d, want 150", got)
	}

	// 3. Stats with < 10 requests falls through to pricing
	got = EstimateCompletionTokens(nil, stats5, pricing)
	if got != 300 {
		t.Errorf("with stats < 10: got %d, want 300", got)
	}

	// 4. Pricing default when no stats
	got = EstimateCompletionTokens(nil, nil, pricing)
	if got != 300 {
		t.Errorf("with pricing only: got %d, want 300", got)
	}

	// 5. Global default when nothing
	got = EstimateCompletionTokens(nil, nil, nil)
	if got != 500 {
		t.Errorf("global default: got %d, want 500", got)
	}
}

func TestEstimateCost(t *testing.T) {
	pricing := &store.CreditPricing{PromptRate: 0.002, CompletionRate: 0.002}

	cost := EstimateCost(pricing, 100, 200)
	expected := 100*0.002 + 200*0.002 // 0.6
	if !almostEqual(cost, expected) {
		t.Errorf("EstimateCost = %f, want %f", cost, expected)
	}

	// Nil pricing returns 0
	if !almostEqual(EstimateCost(nil, 100, 200), 0) {
		t.Error("expected 0 for nil pricing")
	}
}

func TestEstimateFromResponseBytes(t *testing.T) {
	tests := []struct {
		bytes    int
		expected int
	}{
		{400, 100},
		{3, 1},
		{0, 0},
		{1, 1},
	}
	for _, tt := range tests {
		got := EstimateFromResponseBytes(tt.bytes)
		if got != tt.expected {
			t.Errorf("EstimateFromResponseBytes(%d) = %d, want %d", tt.bytes, got, tt.expected)
		}
	}
}
