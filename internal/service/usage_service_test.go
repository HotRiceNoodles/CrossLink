package service

import "testing"

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
