package guardrail

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crosslink/internal/domain"
	"gorm.io/datatypes"
)

func credentialPercentFixture(text string) string {
	var out strings.Builder
	for _, b := range []byte(text) {
		fmt.Fprintf(&out, "%%%02X", b)
	}
	return out.String()
}

func TestCredentialDetectionMasksOriginalEncodedRanges(t *testing.T) {
	secret := credentialFixtures()["sk_bf_key"]
	encoded := base64.StdEncoding.EncodeToString([]byte(secret))
	url := credentialPercentFixture(secret)
	for _, tc := range []struct{ name, value string }{
		{"明文", secret},
		{"URL编码", url},
		{"混合URL编码", "sk%2Dbf%2D" + secret[len("sk-bf-"):]},
		{"Base64编码", encoded},
		{"Base64含CR", encoded[:12] + "\r" + encoded[12:]},
		{"URL安全Base64", base64.URLEncoding.EncodeToString([]byte("😺 " + secret))},
		{"Base64包裹URL", base64.StdEncoding.EncodeToString([]byte(url))},
		{"编码行空白", " \t" + encoded + " \t\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := credentialEngine(t, `{}`)
			prefix, suffix := "普通前文。\n", "\n安全后文。"
			text := prefix + tc.value + suffix
			result, err := engine.Check(t.Context(), text, DirectionRequest, "")
			if err != nil || !result.Blocked || len(result.MaskedSpans) == 0 {
				t.Fatalf("凭据没有定位到原文范围：%+v %v", result, err)
			}
			if strings.Contains(result.MaskedContent, secret) || strings.Contains(result.MaskedContent, tc.value) {
				t.Fatal("脱敏结果仍保留完整凭据或可还原的原始编码")
			}
			if !strings.HasPrefix(result.MaskedContent, prefix) || !strings.HasSuffix(result.MaskedContent, suffix) || strings.Count(result.MaskedContent, prefix) != 1 || strings.Count(result.MaskedContent, suffix) != 1 {
				t.Fatal("脱敏结果改变普通前后文或追加了解码副本")
			}
			if result.MaskedContent != applySpans(text, result.MaskedSpans) {
				t.Fatal("脱敏结果与原文字节范围不一致")
			}
			again, err := engine.Check(t.Context(), result.MaskedContent, DirectionRequest, "")
			if err != nil || again.Blocked {
				t.Fatal("脱敏后仍可检测到完整凭据", err)
			}
		})
	}
}

func TestCredentialDetectionMaskMultipleChannelsAndPatterns(t *testing.T) {
	secret := credentialFixtures()["sk_bf_key"]
	other := credentialFixtures()["github_pat"]
	safeEncoded := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("安全内容", 8)))
	text := "前文：" + secret + "\n" + credentialPercentFixture(secret) + "\n" + safeEncoded + "\n" + base64.StdEncoding.EncodeToString([]byte(other)) + "\n后文。"
	engine := credentialEngine(t, `{"max_matches_per_check":1}`)
	result, err := engine.Check(t.Context(), text, DirectionResponse, "")
	if err != nil || !result.Blocked || len(result.MaskedSpans) < 3 {
		t.Fatalf("多通道凭据未完整脱敏：%+v %v", result, err)
	}
	if !strings.Contains(result.MaskedContent, safeEncoded) {
		t.Fatal("安全编码行被修改")
	}
	again, err := engine.Check(t.Context(), result.MaskedContent, DirectionResponse, "")
	if err != nil || again.Blocked {
		t.Fatal("原因数量限制导致其他凭据未脱敏", err)
	}
}

func TestCredentialDetectionMaskAcrossDecodedLines(t *testing.T) {
	first := base64.StdEncoding.EncodeToString([]byte("-----BEGIN " + strings.Repeat(" ", 20)))
	second := base64.StdEncoding.EncodeToString([]byte("RSA PRIVATE KEY-----" + strings.Repeat(" ", 12)))
	text := "普通前文。\n" + first + "\n中间普通行\n" + second + "\n安全后文。"
	engine := credentialEngine(t, `{}`)
	result, err := engine.Check(t.Context(), text, DirectionRequest, "")
	if err != nil || !result.Blocked || len(result.MaskedSpans) == 0 {
		t.Fatalf("跨解码行的凭据未映射：%+v %v", result, err)
	}
	if strings.Contains(result.MaskedContent, first) || strings.Contains(result.MaskedContent, second) || !strings.HasPrefix(result.MaskedContent, "普通前文。\n") || !strings.HasSuffix(result.MaskedContent, "\n安全后文。") {
		t.Fatal("跨解码行凭据仍能还原或普通外围文本被改变")
	}
}

func TestCredentialMaskServiceUsesOriginalEncodedSpans(t *testing.T) {
	svc := incrementalService()
	svc.rules[0].Type = "credential_detection"
	svc.rules[0].Direction = "both"
	svc.rules[0].Action = "mask"
	svc.rules[0].Config = datatypes.JSON(`{"categories":["api_key"]}`)
	secret := credentialFixtures()["sk_bf_key"]
	for _, text := range []string{credentialPercentFixture(secret), base64.StdEncoding.EncodeToString([]byte(secret))} {
		for _, direction := range []Direction{DirectionRequest, DirectionResponse} {
			result, err := svc.Check(t.Context(), &CheckRequest{Content: text, Model: "test", Direction: direction})
			if err != nil || result.Blocked || result.Action != "mask" || len(result.MaskedSpans) == 0 || strings.Contains(result.MaskedContent, text) || strings.Contains(result.MaskedContent, secret) {
				t.Fatalf("服务层未应用编码凭据脱敏：%+v %v", result, err)
			}
		}
	}
}

func TestCredentialMaskStrictStreamUsesEncodedSpans(t *testing.T) {
	secret := credentialFixtures()["sk_bf_key"]
	for _, encoded := range []string{credentialPercentFixture(secret), base64.StdEncoding.EncodeToString([]byte(secret))} {
		for _, split := range []int{1, len(encoded) / 2, len(encoded) - 1} {
			svc := incrementalService()
			svc.SetStreamMode("strict")
			svc.rules[0].Type = "credential_detection"
			svc.rules[0].Action = "mask"
			svc.rules[0].Config = datatypes.JSON(`{"categories":["api_key"]}`)
			ch := make(chan domain.SSEChunk, 5)
			for _, part := range []string{"普通前文。\n", encoded[:split], encoded[split:], "\n安全后文。"} {
				ch <- codexAcceptanceChunk(0, part)
			}
			ch <- domain.SSEChunk{Done: true}
			close(ch)
			w := NewStreamGuardrailWrapper(ch, svc, "test", 0, 0, 0)
			var out strings.Builder
			for {
				result := w.Next(t.Context())
				if result.Blocked != nil {
					t.Fatal("编码凭据脱敏未能安全应用到流式片段")
				}
				if result.Done {
					break
				}
				if result.Chunk != nil && result.Chunk.Chunk != nil {
					for _, choice := range result.Chunk.Chunk.Choices {
						out.WriteString(choice.Delta.Content)
					}
				}
			}
			if strings.Contains(out.String(), encoded) || strings.Contains(out.String(), secret) || !strings.HasPrefix(out.String(), "普通前文。\n") || !strings.HasSuffix(out.String(), "\n安全后文。") {
				t.Fatal("严格流式输出仍保留凭据或丢失普通前后文")
			}
		}
	}
}

func FuzzCredentialMaskSourceMapping(f *testing.F) {
	for _, secret := range credentialFixtures() {
		f.Add("前文。\n" + credentialPercentFixture(secret) + "\n后文。")
		f.Add(base64.StdEncoding.EncodeToString([]byte(secret)))
	}
	f.Add("普通内容 %5 %zz %\n")
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 2048 || !utf8.ValidString(text) {
			t.Skip()
		}
		parts := credentialMaskParts(text)
		texts := make([]string, 0, len(parts))
		for _, part := range parts {
			texts = append(texts, part.text)
			if part.origins != nil && len(part.origins) != len(part.text) {
				t.Fatal("解码字节与来源映射长度不一致")
			}
			for _, origin := range part.origins {
				if origin.start == -1 && origin.end == -1 {
					continue // 合成的解码行分隔符没有原文位置。
				}
				if origin.start < 0 || origin.end <= origin.start || origin.end > len(text) {
					t.Fatal("来源映射越界")
				}
			}
		}
		if strings.Join(texts, "\n") != preprocessCredentialContent(text) {
			t.Fatal("来源映射改变了既有完整检测的解码语义")
		}
	})
}
