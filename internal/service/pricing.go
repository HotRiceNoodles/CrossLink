package service

import "encoding/json"

type PricingRule struct {
	Type     string  `json:"type"`
	Price    float64 `json:"price"`
	Currency string  `json:"currency"`
}

func ResolvePricing(extraConfig json.RawMessage) PricingRule {
	if len(extraConfig) == 0 {
		return PricingRule{}
	}
	var raw struct {
		Pricing PricingRule `json:"pricing"`
	}
	if json.Unmarshal(extraConfig, &raw) != nil {
		return PricingRule{}
	}
	return raw.Pricing
}
