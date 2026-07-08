package credits

import (
	"log/slog"

	"github.com/krishna/local-ai-proxy/internal/store"
)

// WarnIfPricingEmpty emits a warn-level log when the pricing catalog has no
// active rows. Nothing seeds pricing on a fresh install, and /v1/models only
// lists models that are both priced and served — so an empty catalog means an
// empty model list until the operator adds pricing (POST /api/admin/pricing).
func WarnIfPricingEmpty(db *store.Store, logger *slog.Logger) error {
	pricing, err := db.ListActivePricing()
	if err != nil {
		return err
	}
	if len(pricing) == 0 {
		logger.Warn("pricing catalog is empty — /v1/models will list nothing until pricing is added; see the README's \"Price your models\" step")
	}
	return nil
}
