package service

import (
	"encoding/json"
	"strings"
)

type PricingRule struct {
	Type     string           `json:"type"`
	Price    float64          `json:"price"`
	Currency string           `json:"currency"`
	Tiers    []ImagePriceTier `json:"tiers,omitempty"`
}

type ImagePriceTier struct {
	Size    string  `json:"size"`
	Quality string  `json:"quality"`
	Price   float64 `json:"price"`
}

// ResolveImagePrice picks the unit price for one image: exact (size,quality)
// tier first, then size-only, then quality-only, then the rule's default
// price. Comparisons are case-insensitive; an empty tier field matches any
// request value. Among partial matches, size-only takes precedence over
// quality-only; when multiple tiers of the same kind match, the first
// matching tier in rule order wins.
func ResolveImagePrice(rule *PricingRule, size, quality string) float64 {
	if rule == nil {
		return 0
	}
	if len(rule.Tiers) == 0 {
		return rule.Price
	}
	var sizeMatch, qualityMatch *ImagePriceTier
	for i := range rule.Tiers {
		tier := &rule.Tiers[i]
		if tier.Size != "" && tier.Quality != "" {
			if strings.EqualFold(tier.Size, size) && strings.EqualFold(tier.Quality, quality) {
				return tier.Price
			}
			continue
		}
		if tier.Size != "" && strings.EqualFold(tier.Size, size) {
			if sizeMatch == nil {
				sizeMatch = tier
			}
		}
		if tier.Quality != "" && strings.EqualFold(tier.Quality, quality) {
			if qualityMatch == nil {
				qualityMatch = tier
			}
		}
	}
	if sizeMatch != nil {
		return sizeMatch.Price
	}
	if qualityMatch != nil {
		return qualityMatch.Price
	}
	return rule.Price
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
