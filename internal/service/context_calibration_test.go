package service

import "testing"

func TestCalibrationEMA(t *testing.T) {
	c := NewCalibrationService(nil) // nil redis -> memory only
	for i := 0; i < 50; i++ {
		c.Observe("gpt-4o", 12000, 10000) // actual/estimated = 1.2
	}
	f := c.Factor("gpt-4o")
	if f < 1.15 || f > 1.25 {
		t.Errorf("EMA should converge near 1.2, got %f", f)
	}
	if c.Factor("unknown-family") != 1.0 {
		t.Errorf("uninitialized family factor must be 1.0")
	}
}

func TestCalibrationSkipsInvalid(t *testing.T) {
	c := NewCalibrationService(nil)
	c.Observe("gpt-4o", 0, 10000) // actual 0
	c.Observe("gpt-4o", 100, 0)   // estimated 0
	if c.Factor("gpt-4o") != 1.0 {
		t.Errorf("invalid observations must not move factor")
	}
}

func TestModelFamily(t *testing.T) {
	cases := map[string]string{
		"gpt-4o-2024-11-20": "gpt",
		"gpt-5-mini":        "gpt",
		"claude-sonnet-4-5": "claude",
		"deepseek-chat":     "deepseek",
		"qwen-max":          "qwen",
		"glm-4-plus":        "glm",
		"whatever":          "default",
	}
	for name, want := range cases {
		if got := ModelFamily(name); got != want {
			t.Errorf("ModelFamily(%q) = %q, want %q", name, got, want)
		}
	}
}
