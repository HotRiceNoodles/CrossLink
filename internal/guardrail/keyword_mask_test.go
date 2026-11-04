package guardrail

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func keywordMaskEngine(t *testing.T, config string) *KeywordEngine {
	t.Helper()
	engine, err := NewKeywordEngineFromConfig(json.RawMessage(config))
	if err != nil {
		t.Fatal(err)
	}
	return engine.(*KeywordEngine)
}

func TestKeywordMaskFindsAllOriginalUnicodeRanges(t *testing.T) {
	engine := keywordMaskEngine(t, `{"blocklist":["k秘密","bad","允许"],"allowlist":["允许"]}`)
	result, err := engine.Check(t.Context(), "前K秘密，BAD，bad，允许。", DirectionResponse, "")
	want := []Span{{Start: 3, End: 12, Replacement: "[REDACTED]"}, {Start: 15, End: 18, Replacement: "[REDACTED]"}, {Start: 21, End: 24, Replacement: "[REDACTED]"}}
	if err != nil || !result.Blocked || !reflect.DeepEqual(result.MaskedSpans, want) || result.MaskedContent != "前[REDACTED]，[REDACTED]，[REDACTED]，允许。" {
		t.Fatalf("全部命中未按原文 Unicode 字节坐标脱敏：%+v %v", result, err)
	}
}

func TestKeywordMaskRegexAllowlistDoesNotHideLaterMatches(t *testing.T) {
	engine := keywordMaskEngine(t, `{"blocklist":["id[0-9]+"],"allowlist":["id1"],"use_regex":true}`)
	result, err := engine.Check(t.Context(), "id1 id22 ID333", DirectionResponse, "")
	want := []Span{{Start: 4, End: 8, Replacement: "[REDACTED]"}, {Start: 9, End: 14, Replacement: "[REDACTED]"}}
	if err != nil || !result.Blocked || !reflect.DeepEqual(result.MaskedSpans, want) {
		t.Fatalf("允许列表首个命中遮挡后续正则命中：%+v %v", result, err)
	}
}

func TestKeywordStreamMaskRetainsOverlappingCandidate(t *testing.T) {
	engine := keywordMaskEngine(t, `{"blocklist":["abc","bcde"],"case_sensitive":true}`)
	s, err := engine.NewStream(context.Background(), CheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, err := s.Push(t.Context(), StreamFragment{Key: "text", Text: "前abc"})
	if err != nil || v.Result == nil || !v.Result.Blocked || v.BlockStart == nil || *v.BlockStart != 3 || v.ReleaseThrough != 3 || len(v.Spans) != 0 {
		t.Fatalf("已命中区间与未决候选重叠时提前释放：%+v %v", v, err)
	}
	v, err = s.Push(t.Context(), StreamFragment{Key: "text", Text: "de 后", Final: true})
	want := []Span{{Start: 3, End: 8, Replacement: "[REDACTED]"}}
	if err != nil || v.ReleaseThrough != len("前abcde 后") || !reflect.DeepEqual(v.Spans, want) {
		t.Fatalf("重叠关键词未合并或未继续释放后文：%+v %v", v, err)
	}
}

func TestKeywordStreamMaskEverySplitAndDoesNotRepeatReleasedSpans(t *testing.T) {
	engine := keywordMaskEngine(t, `{"blocklist":["k秘密","BAD"]}`)
	text := "前K秘密，bad 后"
	runes := []rune(text)
	for split := 0; split <= len(runes); split++ {
		s, err := engine.NewStream(t.Context(), CheckRequest{})
		if err != nil {
			t.Fatal(err)
		}
		var got []Span
		for i, part := range []string{string(runes[:split]), string(runes[split:]), "。"} {
			v, err := s.Push(t.Context(), StreamFragment{Text: part, Final: i == 2})
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, v.Spans...)
		}
		s.Close()
		want := []Span{{Start: 3, End: 12, Replacement: "[REDACTED]"}, {Start: 15, End: 18, Replacement: "[REDACTED]"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("split=%d：跨片段脱敏遗漏或重复：%+v", split, got)
		}
	}
}
