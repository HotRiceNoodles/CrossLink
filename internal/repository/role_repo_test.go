package repository

import (
	"testing"
)

func TestDedupActions(t *testing.T) {
	tests := []struct {
		name    string
		actions []string
		want    []string
	}{
		{"empty", nil, nil},
		{"no duplicates", []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"with duplicates", []string{"a", "b", "a", "c", "b"}, []string{"a", "b", "c"}},
		{"all same", []string{"x", "x", "x"}, []string{"x"}},
		{"single", []string{"only"}, []string{"only"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dedupActions(tt.actions)
			if len(got) != len(tt.want) {
				t.Fatalf("dedupActions() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("dedupActions()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
