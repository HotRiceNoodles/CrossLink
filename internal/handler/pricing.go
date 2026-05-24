package handler

import (
	"encoding/json"

	"github.com/crosslink/internal/service"
)

type PricingRule = service.PricingRule

func resolvePricing(extraConfig json.RawMessage) PricingRule {
	return service.ResolvePricing(extraConfig)
}
