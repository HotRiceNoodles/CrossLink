package guardrail

import (
	"testing"
)

func TestModelInFilter(t *testing.T) {
	tests := []struct {
		name   string
		model  string
		filter string
		want   bool
	}{
		{"empty filter", "gpt-4", "", false},
		{"single match", "gpt-4", "gpt-4", true},
		{"single no match", "gpt-4", "claude-3", false},
		{"comma first", "gpt-4", "gpt-4,claude-3", true},
		{"comma last", "claude-3", "gpt-4,claude-3", true},
		{"comma middle", "qwen", "gpt-4,qwen,claude-3", true},
		{"spaces", "gpt-4", "gpt-4, claude-3", true},
		{"empty model", "", "gpt-4", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := modelInFilter(tt.model, tt.filter)
			if got != tt.want {
				t.Errorf("modelInFilter(%q, %q) = %v, want %v", tt.model, tt.filter, got, tt.want)
			}
		})
	}
}
