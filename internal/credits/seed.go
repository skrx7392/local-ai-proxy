package credits

import "github.com/krishna/local-ai-proxy/internal/store"

// SeedDefaultPricing inserts the default model pricing. Idempotent via ON CONFLICT DO NOTHING.
func SeedDefaultPricing(db *store.Store) error {
	models := []struct {
		modelID           string
		promptRate        float64
		completionRate    float64
		typicalCompletion int
	}{
		// Small tier (1x)
		{"qwen2.5-coder:1.5b", 0.001, 0.001, 300},
		{"gemma4:e2b", 0.001, 0.001, 300},
		// Medium tier (2x)
		{"deepseek-coder:6.7b", 0.002, 0.002, 500},
		{"qwen2.5-coder:7b", 0.002, 0.002, 500},
		{"llama3.1:8b", 0.002, 0.002, 500},
		// Large tier (4x)
		{"gemma4:e4b", 0.004, 0.004, 500},
		{"qwen2.5:14b-instruct", 0.004, 0.004, 500},
	}

	for _, m := range models {
		if err := db.UpsertPricing(m.modelID, m.promptRate, m.completionRate, m.typicalCompletion); err != nil {
			return err
		}
	}
	return nil
}
