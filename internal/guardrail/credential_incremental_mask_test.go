package guardrail

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

func TestCredentialStreamMaskWaitsForVariableLengthBoundary(t *testing.T) {
	engine := credentialEngine(t, `{"categories":["api_key"],"mask_keep_prefix":3,"mask_keep_suffix":2,"mask_char":"#"}`)
	s := credentialSession(t, engine)
	prefix := "提示："
	secret := "sk-" + strings.Repeat("Q", 25)
	v, err := s.Push(t.Context(), StreamFragment{Text: prefix + secret[:23]})
	if err != nil || v.Result == nil || !v.Result.Blocked || v.BlockStart == nil || *v.BlockStart != len(prefix) || v.ReleaseThrough != len(prefix) || len(v.Spans) != 0 {
		t.Fatalf("可变长凭据未在确认阻断后继续保留脱敏候选：%+v %v", v, err)
	}
	v, err = s.Push(t.Context(), StreamFragment{Text: secret[23:] + " 后文。"})
	want := []Span{{Start: len(prefix), End: len(prefix + secret), Replacement: "sk-" + strings.Repeat("#", 23) + "QQ"}}
	if err != nil || !reflect.DeepEqual(v.Spans, want) || v.ReleaseThrough != len(prefix+secret+" 后文。") {
		t.Fatalf("未在真实边界按完整凭据保留配置前后缀：%+v %v", v, err)
	}
	v, err = s.Push(t.Context(), StreamFragment{Text: "结束。", Final: true})
	if err != nil || len(v.Spans) != 0 || v.Result != nil {
		t.Fatalf("已释放的凭据替换被重复报告：%+v %v", v, err)
	}
}

func TestCredentialStreamLowConfidenceMasksWithoutBlocking(t *testing.T) {
	engine := credentialEngine(t, `{"categories":["env_file"]}`)
	s := credentialSession(t, engine)
	v, err := s.Push(t.Context(), StreamFragment{Text: "PASSWORD=abcdefgh12345678", Final: true})
	want := []Span{{Start: 0, End: 25, Replacement: "PASS" + strings.Repeat("*", 17) + "5678"}}
	if err != nil || v.Result == nil || v.Result.Blocked || v.BlockStart != nil || !reflect.DeepEqual(v.Spans, want) || v.ReleaseThrough != 25 {
		t.Fatalf("低置信度凭据未保留脱敏且不升级阻断：%+v %v", v, err)
	}
}

func TestCredentialStreamMaskEncodedOriginalRangesAndContinuation(t *testing.T) {
	secret := "sk-" + strings.Repeat("Q", 30)
	masked := "sk-Q" + strings.Repeat("*", 25) + "QQQQ"
	for _, encoded := range []string{"%73%6b%2d" + strings.Repeat("%51", 30), base64.StdEncoding.EncodeToString([]byte(secret))} {
		for step := 1; step <= 5; step++ {
			s := credentialSession(t, credentialEngine(t, `{"categories":["api_key"]}`))
			var spans []Span
			for start := 0; start < len(encoded); start += step {
				v, err := s.Push(t.Context(), StreamFragment{Text: encoded[start:min(start+step, len(encoded))]})
				if err != nil {
					t.Fatal(err)
				}
				spans = append(spans, v.Spans...)
				if v.ReleaseThrough != 0 {
					t.Fatalf("编码凭据候选提前放行：%+v", v)
				}
			}
			v, err := s.Push(t.Context(), StreamFragment{Text: "\n后文。", Final: true})
			if err != nil {
				t.Fatal(err)
			}
			spans = append(spans, v.Spans...)
			want := []Span{{Start: 0, End: len(encoded), Replacement: masked}}
			if !reflect.DeepEqual(spans, want) || v.ReleaseThrough != len(encoded+"\n后文。") {
				t.Fatalf("编码匹配未映射原文并继续：step=%d %+v %+v", step, spans, v)
			}
		}
	}
}

func TestCredentialStreamMaskFixedLengthAndAllHits(t *testing.T) {
	engine := credentialEngine(t, `{"categories":["cloud_credential"]}`)
	s := credentialSession(t, engine)
	secret := "AKIA" + strings.Repeat("Q", 16)
	v, err := s.Push(t.Context(), StreamFragment{Text: "前：" + secret + " 后：" + secret, Final: true})
	want := []Span{{Start: 6, End: 26, Replacement: "AKIA" + strings.Repeat("*", 12) + "QQQQ"}, {Start: 33, End: 53, Replacement: "AKIA" + strings.Repeat("*", 12) + "QQQQ"}}
	if err != nil || v.BlockStart == nil || *v.BlockStart != 6 || !reflect.DeepEqual(v.Spans, want) || v.ReleaseThrough != 53 {
		t.Fatalf("固定长度凭据未返回全部命中及最早阻断坐标：%+v %v", v, err)
	}
}

func TestCredentialStreamLowConfidenceLineAnchorSurvivesFragments(t *testing.T) {
	engine := credentialEngine(t, `{"categories":["env_file"]}`)
	s := credentialSession(t, engine)
	if _, err := s.Push(t.Context(), StreamFragment{Text: "same line: "}); err != nil {
		t.Fatal(err)
	}
	v, err := s.Push(t.Context(), StreamFragment{Text: "PASSWORD=abcdefgh12345678", Final: true})
	if err != nil || len(v.Spans) != 0 || v.Result != nil {
		t.Fatalf("片段开始被误当成 env 行起点：%+v %v", v, err)
	}
}

func TestCredentialStreamMaskMatchesCompleteAcrossEverySplit(t *testing.T) {
	engine := credentialEngine(t, `{}`)
	fixtures := credentialFixtures()
	fixtures["env_secret"] = "PASSWORD=abcdefgh12345678"
	for name, secret := range fixtures {
		t.Run(name, func(t *testing.T) {
			prefix := "普通前文："
			if name == "env_secret" {
				prefix += "\n"
			}
			text := prefix + secret + "，后文。"
			whole, err := engine.Check(t.Context(), text, DirectionResponse, "")
			if err != nil || len(whole.MaskedSpans) == 0 {
				t.Fatalf("完整脱敏基准未识别：%+v %v", whole, err)
			}
			runes := []rune(text)
			for split := -1; split <= len(runes); split++ {
				s := credentialSession(t, engine)
				parts := []string{string(runes[:max(split, 0)]), string(runes[max(split, 0):])}
				if split == -1 {
					parts = nil
					for _, r := range runes {
						parts = append(parts, string(r))
					}
				}
				parts = append(parts, "")
				var spans []Span
				released := 0
				for i, part := range parts {
					v, err := s.Push(t.Context(), StreamFragment{Text: part, Final: i == len(parts)-1})
					if err != nil {
						t.Fatal(err)
					}
					for _, span := range v.Spans {
						if span.Start < released {
							t.Fatalf("split=%d: 重复或追溯替换：%+v", split, v)
						}
						spans = append(spans, span)
					}
					if v.ReleaseThrough < released {
						t.Fatalf("split=%d: 水位回退：%+v", split, v)
					}
					released = v.ReleaseThrough
				}
				if got := applySpans(text, spans); got != whole.MaskedContent || released != len(text) {
					t.Fatalf("split=%d: 增量脱敏与完整基准不一致：got=%q want=%q spans=%+v release=%d", split, got, whole.MaskedContent, spans, released)
				}
			}
		})
	}
}

func TestCredentialStreamInactiveCategoriesReleaseImmediately(t *testing.T) {
	s := credentialSession(t, credentialEngine(t, `{"categories":["inactive"]}`))
	v, err := s.Push(t.Context(), StreamFragment{Text: "YQ=="})
	if err != nil || v.Result != nil || v.ReleaseThrough != 4 {
		t.Fatalf("无适用凭据模式仍暂留候选：%+v %v", v, err)
	}
}
