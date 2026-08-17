package service

import (
	"testing"

	"github.com/crosslink/internal/model"
)

func TestUsageEntry_CostCalculation(t *testing.T) {
	tests := []struct {
		name        string
		inputPrice  float64
		outputPrice float64
		inputTokens int
		outputTok   int
		wantCost    float64
	}{
		{"zero prices", 0, 0, 100, 50, 0},
		{"input only", 0.001, 0, 1000, 0, 0.001},
		{"output only", 0, 0.002, 0, 500, 0.001},
		{"both", 0.0014, 0.0028, 1000, 500, 0.0028},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := &UsageEntry{
				InputTokens:  tt.inputTokens,
				OutputTokens: tt.outputTok,
				InputPrice:   tt.inputPrice,
				OutputPrice:  tt.outputPrice,
			}
			cost := entry.InputPrice*float64(entry.InputTokens)/1000 + entry.OutputPrice*float64(entry.OutputTokens)/1000
			if cost != tt.wantCost {
				t.Errorf("cost = %f, want %f", cost, tt.wantCost)
			}
		})
	}
}

func TestBuildUsageLog(t *testing.T) {
	tplID := int64(7)
	tests := []struct {
		name string
		entry UsageEntry
		check func(t *testing.T, log *model.UsageLog)
	}{
		{"image metadata populated", UsageEntry{
			ImageCount: 3, ImageSize: "1024x1024", ImageQuality: "HD",
		}, func(t *testing.T, log *model.UsageLog) {
			if log.ImageCount == nil || *log.ImageCount != 3 {
				t.Errorf("ImageCount = %v, want 3", log.ImageCount)
			}
			if log.ImageSize == nil || *log.ImageSize != "1024x1024" {
				t.Errorf("ImageSize = %v, want 1024x1024", log.ImageSize)
			}
			if log.ImageQuality == nil || *log.ImageQuality != "HD" {
				t.Errorf("ImageQuality = %v, want HD", log.ImageQuality)
			}
		}},
		{"empty image values stay nil", UsageEntry{}, func(t *testing.T, log *model.UsageLog) {
			if log.ImageCount != nil || log.ImageSize != nil || log.ImageQuality != nil {
				t.Errorf("image fields should be nil, got %v/%v/%v", log.ImageCount, log.ImageSize, log.ImageQuality)
			}
		}},
		{"precomputed cost and multiplier", UsageEntry{
			PrecomputedCost: 0.5, PriceMultiplier: 2.0,
		}, func(t *testing.T, log *model.UsageLog) {
			if log.Cost != 0.5 {
				t.Errorf("Cost = %v, want 0.5", log.Cost)
			}
			if log.BillableCost != 1.0 {
				t.Errorf("BillableCost = %v, want 1.0", log.BillableCost)
			}
		}},
		{"multiplier defaults to 1", UsageEntry{
			PrecomputedCost: 0.5,
		}, func(t *testing.T, log *model.UsageLog) {
			if log.BillableCost != 0.5 {
				t.Errorf("BillableCost = %v, want 0.5", log.BillableCost)
			}
		}},
		{"template id passthrough", UsageEntry{TemplateID: &tplID}, func(t *testing.T, log *model.UsageLog) {
			if log.TemplateID == nil || *log.TemplateID != 7 {
				t.Errorf("TemplateID = %v, want 7", log.TemplateID)
			}
		}},
		{"empty strings stay nil", UsageEntry{}, func(t *testing.T, log *model.UsageLog) {
			if log.UserMessage != nil || log.ModelResponse != nil {
				t.Errorf("content fields should be nil")
			}
		}},
		{"zero ids stay nil", UsageEntry{}, func(t *testing.T, log *model.UsageLog) {
			if log.ProviderID != nil || log.APIKeyID != nil || log.TeamID != nil || log.OrgID != nil || log.FirstTokenMs != nil {
				t.Errorf("optional ids should be nil")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := tt.entry
			tt.check(t, buildUsageLog(&entry))
		})
	}
}
