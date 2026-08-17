package service

import (
	"encoding/json"
	"testing"
)

func TestResolveImagePrice(t *testing.T) {
	tests := []struct {
		name     string
		rule     PricingRule
		size     string
		quality  string
		expected float64
	}{
		{"exact match", PricingRule{Price: 0.04, Tiers: []ImagePriceTier{
			{Size: "1024x1024", Quality: "standard", Price: 0.04},
			{Size: "1024x1024", Quality: "hd", Price: 0.08},
		}}, "1024x1024", "hd", 0.08},
		{"case-insensitive", PricingRule{Price: 0.04, Tiers: []ImagePriceTier{
			{Size: "1024x1024", Quality: "hd", Price: 0.08},
		}}, "1024X1024", "HD", 0.08},
		{"size-only tier matches any quality", PricingRule{Price: 0.04, Tiers: []ImagePriceTier{
			{Size: "1792x1024", Price: 0.12},
		}}, "1792x1024", "hd", 0.12},
		{"quality-only tier", PricingRule{Price: 0.04, Tiers: []ImagePriceTier{
			{Quality: "hd", Price: 0.09},
		}}, "1024x1024", "hd", 0.09},
		{"no tier match falls back to default price", PricingRule{Price: 0.04, Tiers: []ImagePriceTier{
			{Size: "1024x1024", Quality: "standard", Price: 0.04},
		}}, "512x512", "standard", 0.04},
		{"no tiers returns default price (legacy regression guard)", PricingRule{Price: 0.04}, "1024x1024", "hd", 0.04},
		{"no tiers zero price returns zero", PricingRule{}, "1024x1024", "hd", 0},
		{"empty request size and quality", PricingRule{Price: 0.05, Tiers: []ImagePriceTier{
			{Size: "1024x1024", Quality: "standard", Price: 0.04},
		}}, "", "", 0.05},
		{"first size-only tier wins on duplicate size", PricingRule{Price: 0.05, Tiers: []ImagePriceTier{
			{Size: "1024x1024", Price: 0.04},
			{Size: "1024x1024", Price: 0.06},
		}}, "1024x1024", "hd", 0.04},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveImagePrice(&tt.rule, tt.size, tt.quality)
			if got != tt.expected {
				t.Errorf("ResolveImagePrice(%+v, %q, %q) = %v, want %v", tt.rule, tt.size, tt.quality, got, tt.expected)
			}
		})
	}
}

func TestResolveImagePriceNilRule(t *testing.T) {
	if got := ResolveImagePrice(nil, "1024x1024", "hd"); got != 0 {
		t.Errorf("ResolveImagePrice(nil, ...) = %v, want 0", got)
	}
}

func TestPricingRuleUnmarshalTiers(t *testing.T) {
	raw := json.RawMessage(`{"pricing":{"type":"per_image","price":0.04,"currency":"USD","tiers":[{"size":"1024x1024","quality":"hd","price":0.08}]}}`)
	rule := ResolvePricing(raw)
	if rule.Price != 0.04 || len(rule.Tiers) != 1 || rule.Tiers[0].Price != 0.08 {
		t.Fatalf("unexpected rule: %+v", rule)
	}
}
