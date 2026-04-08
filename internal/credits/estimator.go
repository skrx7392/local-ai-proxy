package credits

import "github.com/krishna/local-ai-proxy/internal/store"

const globalDefaultCompletion = 500

// EstimatePromptTokens estimates prompt tokens from request body length.
// ~4 chars per token, JSON overhead makes this slightly conservative.
func EstimatePromptTokens(bodyLen int) int {
	tokens := bodyLen / 4
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

// EstimateCompletionTokens estimates completion tokens using a cascade:
// maxTokens -> historical average (10+ requests) -> model default -> global default.
func EstimateCompletionTokens(maxTokens *int, stats *store.AccountUsageStats, pricing *store.CreditPricing) int {
	if maxTokens != nil && *maxTokens > 0 {
		return *maxTokens
	}
	if stats != nil && stats.RequestCount >= 10 {
		return stats.AvgCompletionTokens
	}
	if pricing != nil && pricing.TypicalCompletion > 0 {
		return pricing.TypicalCompletion
	}
	return globalDefaultCompletion
}

// EstimateCost calculates the credit cost from token counts and pricing.
func EstimateCost(pricing *store.CreditPricing, promptTokens, completionTokens int) float64 {
	if pricing == nil {
		return 0
	}
	return float64(promptTokens)*pricing.PromptRate + float64(completionTokens)*pricing.CompletionRate
}

// EstimateFromResponseBytes estimates tokens from response body size.
// Used as fallback when token extraction fails.
func EstimateFromResponseBytes(bytesWritten int) int {
	tokens := bytesWritten / 4
	if tokens < 1 && bytesWritten > 0 {
		tokens = 1
	}
	return tokens
}
