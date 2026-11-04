package guardrail

import "testing"

func TestLengthStreamReleasesLegalPrefixWithinCrossingFragment(t *testing.T) {
	for _, tc := range []struct {
		prefix, fragment string
		limit, start     int
	}{
		{"", "你🙂好ab", 3, len("你🙂好")},
		{"你", "🙂好ab", 3, len("你🙂好")},
		{"你🙂好", "ab", 3, len("你🙂好")},
	} {
		s := lengthSession(t, tc.limit)
		if tc.prefix != "" {
			if _, err := s.Push(t.Context(), StreamFragment{Key: "text", Text: tc.prefix}); err != nil {
				t.Fatal(err)
			}
		}
		v, err := s.Push(t.Context(), StreamFragment{Key: "text", Text: tc.fragment})
		if err != nil || v.Result == nil || !v.Result.Blocked || v.BlockStart == nil || *v.BlockStart != tc.start || v.ReleaseThrough != tc.start {
			t.Fatalf("跨限片段未准确批准前 N 个原文字符：%+v %v", v, err)
		}
	}
}

func TestLengthStreamSeparatorExhaustsNewFieldBudget(t *testing.T) {
	s := lengthSession(t, 3)
	if _, err := s.Push(t.Context(), StreamFragment{Key: "one", Text: "你好"}); err != nil {
		t.Fatal(err)
	}
	v, err := s.Push(t.Context(), StreamFragment{Key: "two", Text: "🙂后"})
	if err != nil || v.Result == nil || v.BlockStart == nil || *v.BlockStart != 0 || v.ReleaseThrough != 0 {
		t.Fatalf("字段分隔符未占用整条响应预算：%+v %v", v, err)
	}
}
