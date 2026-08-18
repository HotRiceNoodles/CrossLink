package token

import "testing"

func TestEstimateZeroAlloc(t *testing.T) {
	// Must agree with the original Estimate on pure ASCII, pure CJK and mixed.
	cases := []string{"hello world", "你好世界", "混合mixed文本text123", "", "    "}
	for _, s := range cases {
		if got, want := EstimateZeroAlloc(s), Estimate(s); got != want {
			t.Errorf("EstimateZeroAlloc(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestEstimate_ASCII(t *testing.T) {
	// 20 ASCII chars -> ~5 tokens
	got := Estimate("Hello world, test!")
	if got == 0 {
		t.Error("should return non-zero for non-empty text")
	}
}

func TestEstimate_CJK(t *testing.T) {
	// 6 CJK chars -> ~4 tokens (6 * 2/3 = 4)
	got := Estimate("你好世界测试")
	if got < 3 {
		t.Errorf("expected ~4 tokens for 6 CJK chars, got %d", got)
	}
}

func TestEstimate_Mixed(t *testing.T) {
	// 4 CJK + 11 ASCII
	got := Estimate("Hello 你好世界 test")
	if got == 0 {
		t.Error("should return non-zero for mixed text")
	}
}

func TestEstimate_Empty(t *testing.T) {
	got := Estimate("")
	if got != 0 {
		t.Errorf("expected 0 for empty text, got %d", got)
	}
}

func TestEstimate_CJK_HigherThanOld(t *testing.T) {
	text := "这是一个中文测试句子"
	newResult := Estimate(text)
	oldResult := len([]rune(text)) / 4
	if newResult <= oldResult {
		t.Errorf("CJK estimate (%d) should be higher than old runes/4 (%d)", newResult, oldResult)
	}
}
