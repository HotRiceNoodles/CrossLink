package service

import "testing"

func TestLatencyBucket(t *testing.T) {
	tests := []struct{ ms, want int }{
		{0, 0}, {49, 0}, {50, 1}, {99, 1}, {100, 2},
		{199, 2}, {200, 3}, {499, 3}, {500, 4}, {999, 4},
		{1000, 5}, {1999, 5}, {2000, 6}, {4999, 6}, {5000, 7}, {10000, 7},
	}
	for _, tt := range tests {
		if got := LatencyBucket(tt.ms); got != tt.want {
			t.Errorf("LatencyBucket(%d) = %d, want %d", tt.ms, got, tt.want)
		}
	}
}

func TestApproxPercentile(t *testing.T) {
	buckets := []int{100, 80, 60, 40, 20, 10, 5, 1} // 316 total

	p50 := ApproxPercentile(buckets, 50)
	if p50 <= 0 || p50 > 100 {
		t.Errorf("P50 = %d, expected between 1-100", p50)
	}

	p99 := ApproxPercentile(buckets, 99)
	if p99 <= 1000 {
		t.Errorf("P99 = %d, expected > 1000", p99)
	}

	// Edge cases
	empty := []int{}
	if got := ApproxPercentile(empty, 50); got != 0 {
		t.Errorf("Empty buckets should return 0, got %d", got)
	}
	allZero := []int{0, 0, 0, 0, 0, 0, 0, 0}
	if got := ApproxPercentile(allZero, 50); got != 0 {
		t.Errorf("All-zero buckets should return 0, got %d", got)
	}
}

func TestStatusGroup(t *testing.T) {
	tests := []struct {
		code int
		want int
	}{
		{200, 200}, {201, 200}, {299, 200},
		{400, 400}, {401, 400}, {499, 400},
		{429, 429},
		{500, 500}, {502, 500}, {599, 500},
		{100, 0},
	}
	for _, tt := range tests {
		if got := StatusGroup(tt.code); got != tt.want {
			t.Errorf("StatusGroup(%d) = %d, want %d", tt.code, got, tt.want)
		}
	}
}

func TestDefaultLevels(t *testing.T) {
	levels := DefaultLevels()
	if len(levels) != 7 {
		t.Fatalf("Expected 7 levels, got %d", len(levels))
	}
	if levels[0].Name != "global" {
		t.Errorf("First level should be 'global', got %s", levels[0].Name)
	}
	if len(levels[0].Dimensions) != 0 {
		t.Errorf("Global level should have no dimensions")
	}
	if levels[1].Name != "by_model" {
		t.Errorf("Second level should be 'by_model', got %s", levels[1].Name)
	}
}
