package behavior

import (
	"testing"
)

func TestBinOf(t *testing.T) {
	cases := []struct {
		x   float64
		bin int32
	}{
		{1, 0}, {2, 8}, {1024, 80}, {1000, 79}, {8000, 103},
	}
	for _, c := range cases {
		if got := BinOf(c.x); got != c.bin {
			t.Errorf("BinOf(%v) = %d, want %d", c.x, got, c.bin)
		}
	}
}

func TestHistogramAddIgnoresNonPositive(t *testing.T) {
	h := NewLogHistogram()
	h.Add(0)
	h.Add(-5)
	if h.Count() != 0 {
		t.Fatalf("non-positive values must be dropped, got %d", h.Count())
	}
}

func TestHistogramQuantile(t *testing.T) {
	h := NewLogHistogram()
	for i := 0; i < 100; i++ {
		h.Add(1000)
	}
	for i := 0; i < 10; i++ {
		h.Add(8000)
	}
	if h.Count() != 110 {
		t.Fatalf("count = %d, want 110", h.Count())
	}
	// 中位数落在 1000ms 桶内（946.6 下界，桶内插值）。
	q := h.Quantile(0.5)
	if q < 950 || q > 1010 {
		t.Errorf("median = %v, want in (950, 1010)", q)
	}
	// 高分位落 8000 桶。
	q = h.Quantile(0.95)
	if q < 7600 || q > 8400 {
		t.Errorf("p95 = %v, want in (7600, 8400)", q)
	}
	spread := h.QuartileSpread()
	if spread < 15 || spread > 35 {
		t.Errorf("quartile spread = %v, want in (15, 35)", spread)
	}
}

func TestHistogramMergeAndRoundtrip(t *testing.T) {
	h1 := NewLogHistogram()
	for i := 0; i < 100; i++ {
		h1.Add(1000)
	}
	h2 := NewLogHistogram()
	for i := 0; i < 10; i++ {
		h2.Add(8000)
	}
	h1.Merge(h2)
	if h1.Count() != 110 {
		t.Fatalf("merged count = %d, want 110", h1.Count())
	}

	data, err := h1.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := ParseLogHistogram(string(data))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Count() != h1.Count() {
		t.Fatalf("roundtrip count = %d, want %d", parsed.Count(), h1.Count())
	}
	a, b := h1.Quantile(0.5), parsed.Quantile(0.5)
	if a != b {
		t.Fatalf("roundtrip median changed: %v → %v", a, b)
	}
	// 空直方图序列化为 "[]" 且解析回空。
	empty, _ := ParseLogHistogram("")
	if empty.Count() != 0 {
		t.Fatalf("empty parse count = %d", empty.Count())
	}
}

func TestHistogramQuantileEmptyAndBounds(t *testing.T) {
	h := NewLogHistogram()
	if h.Quantile(0.5) != 0 {
		t.Fatalf("empty median = %v, want 0", h.Quantile(0.5))
	}
	h.Add(5)
	h.Add(6)
	if q := h.Quantile(1); q <= 0 {
		t.Fatalf("q=1 quantile = %v, want > 0", q)
	}
	if q := h.Quantile(0); q != 0 {
		t.Fatalf("q=0 quantile = %v, want 0", q)
	}
}
