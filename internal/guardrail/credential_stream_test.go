package guardrail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"gorm.io/datatypes"
)

func credentialEngine(t testing.TB, config string) GuardrailEngine {
	t.Helper()
	engine, err := NewCredentialDetectionEngineFromConfig(json.RawMessage(config))
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func credentialSession(t testing.TB, engine GuardrailEngine) StreamSession {
	t.Helper()
	s, err := engine.(StreamingEngine).NewStream(context.Background(), CheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func credentialFixtures() map[string]string {
	return map[string]string{
		"openai_key":          "sk-" + strings.Repeat("Q", 20),
		"sk_bf_key":           "sk-bf-" + "01234567-89ab-cdef-0123-456789abcdef",
		"anthropic_key":       "sk-ant-" + strings.Repeat("Q", 20),
		"stripe_live_key":     "sk_live_" + strings.Repeat("Q", 20),
		"stripe_test_key":     "sk_test_" + strings.Repeat("Q", 20),
		"github_pat":          "ghp_" + strings.Repeat("Q", 36),
		"github_oauth":        "gho_" + strings.Repeat("Q", 36),
		"slack_bot_token":     "xoxb-" + "1234567890-1234567890-" + strings.Repeat("Q", 24),
		"slack_user_token":    "xoxp-" + "1234567890-1234567890-1234567890-" + strings.Repeat("Q", 24),
		"bearer_token":        "Bearer " + strings.Repeat("Q", 24),
		"generic_api_key":     "api_key='" + strings.Repeat("Q", 32) + "'",
		"aws_access_key":      "AKIA" + strings.Repeat("Q", 16),
		"aws_secret_key":      "aws_secret='" + strings.Repeat("Q", 40) + "'",
		"aliyun_ak":           "LTAI" + strings.Repeat("Q", 12),
		"gcp_service_account": `"type": "service_account"`,
		"azure_conn_string":   "DefaultEndpointsProtocol=https;AccountName=alpha;AccountKey=QQ",
		"private_key":         "-----BEGIN " + "RSA PRIVATE KEY-----",
		"mysql_conn":          "mysql://alpha:pw@host",
		"postgres_conn":       "postgresql://alpha:pw@host",
		"mongodb_conn":        "mongodb+srv://alpha:pw@host",
		"redis_conn":          "redis://:pw@host",
		"jwt_token":           "eyJ" + strings.Repeat("Q", 20) + ".eyJ" + strings.Repeat("R", 20) + "." + strings.Repeat("S", 20),
	}
}

func TestIncrementalCredentialBuiltinsAcrossEverySplit(t *testing.T) {
	engine := credentialEngine(t, `{}`)
	for name, secret := range credentialFixtures() {
		t.Run(name, func(t *testing.T) {
			prefix := "普通前文："
			text := prefix + secret + "，后文。"
			whole, _ := engine.Check(t.Context(), text, DirectionResponse, "")
			if !whole.Blocked {
				t.Fatal("完整检查未识别测试凭据")
			}
			runes := []rune(text)
			for split := -1; split <= len(runes); split++ {
				s := credentialSession(t, engine)
				var fragments []string
				if split < 0 {
					for _, r := range runes {
						fragments = append(fragments, string(r))
					}
				} else {
					fragments = []string{string(runes[:split]), string(runes[split:])}
				}
				fragments = append(fragments, "")
				blocked, release := false, 0
				for i, fragment := range fragments {
					v, err := s.Push(t.Context(), StreamFragment{Text: fragment, Final: i == len(fragments)-1})
					if err != nil {
						t.Fatal(err)
					}
					if v.Result != nil && v.Result.Blocked {
						blocked = true
						if v.Result.ContentSnippet == "" || strings.Contains(v.Result.Reason, secret) || strings.Contains(v.Result.ContentSnippet, secret) {
							t.Fatal("事件缺少安全预览或包含凭据原值")
						}
						break
					}
					if v.ReleaseThrough < release || v.ReleaseThrough > len(prefix) {
						t.Fatalf("split=%d: 凭据前缀提前放行或水位回退：%+v", split, v)
					}
					release = v.ReleaseThrough
				}
				if !blocked {
					t.Fatalf("split=%d: 跨片段凭据漏拦", split)
				}
			}
		})
	}
}

func TestIncrementalCredentialReleaseAndMediumValidation(t *testing.T) {
	engine := credentialEngine(t, `{}`)
	s := credentialSession(t, engine)
	total := 0
	for _, tc := range []struct{ text, tail string }{
		{"普通文字立即输出。", ""}, {"候选 sk-", "sk-"}, {"!", ""},
		{"Bearer " + strings.Repeat("Q", 24), "Bearer " + strings.Repeat("Q", 24)},
		{"example ", ""}, {"普通正文。", ""},
	} {
		total += len(tc.text)
		v, err := s.Push(t.Context(), StreamFragment{Text: tc.text})
		if err != nil || v.Result != nil || v.ReleaseThrough != total-len(tc.tail) {
			t.Fatalf("候选排除或占位符校验错误：%q %+v %v", tc.text, v, err)
		}
	}
	text := "Bearer your_placeholder_key_here " + "Bearer " + strings.Repeat("Q", 24) + " "
	whole, _ := engine.Check(t.Context(), text, DirectionResponse, "")
	if !whole.Blocked {
		t.Fatal("占位符遮挡后续真实凭据")
	}
	s = credentialSession(t, engine)
	v, err := s.Push(t.Context(), StreamFragment{Text: text, Final: true})
	if err != nil || v.Result == nil || !v.Result.Blocked {
		t.Fatal(v, err)
	}
}

func TestIncrementalCredentialSKBFImmediateBlock(t *testing.T) {
	engine := credentialEngine(t, `{"categories":["api_key"]}`)
	s := credentialSession(t, engine)
	prefix := "普通说明，"
	v, err := s.Push(t.Context(), StreamFragment{Text: prefix})
	if err != nil || v.Result != nil || v.ReleaseThrough != len(prefix) {
		t.Fatalf("普通前文未立即放行：%+v %v", v, err)
	}
	secret := credentialFixtures()["sk_bf_key"]
	for i := range len(secret) {
		v, err = s.Push(t.Context(), StreamFragment{Text: secret[i : i+1]})
		if err != nil {
			t.Fatal(err)
		}
		if i < len(secret)-1 {
			if v.Result != nil || v.ReleaseThrough != len(prefix) {
				t.Fatalf("第 %d 个字符的凭据候选提前放行或被提前阻断：%+v", i+1, v)
			}
		} else if v.Result == nil || !v.Result.Blocked || v.Result.Severity != "critical" || v.Result.ContentSnippet != "[CREDENTIAL:sk_bf_key]" {
			t.Fatalf("完整 UUID 未立即阻断：%+v", v)
		}
	}
}

func TestIncrementalCredentialSKBFReleasesInvalidCandidate(t *testing.T) {
	engine := credentialEngine(t, `{}`)
	secret := credentialFixtures()["sk_bf_key"]
	text := "普通说明，" + secret[:len(secret)-1]
	for _, final := range []bool{false, true} {
		t.Run(fmt.Sprintf("final=%v", final), func(t *testing.T) {
			s := credentialSession(t, engine)
			v, err := s.Push(t.Context(), StreamFragment{Text: text})
			if err != nil || v.Result != nil || v.ReleaseThrough != len("普通说明，") {
				t.Fatalf("未完成的 UUID 候选未暂留：%+v %v", v, err)
			}
			tail := "!"
			if final {
				tail = ""
			}
			v, err = s.Push(t.Context(), StreamFragment{Text: tail, Final: final})
			if err != nil || v.Result != nil || v.ReleaseThrough != len(text)+len(tail) {
				t.Fatalf("排除或结束的候选未完整放行：%+v %v", v, err)
			}
		})
	}
}

func TestIncrementalCredentialEncodedChannels(t *testing.T) {
	engine := credentialEngine(t, `{}`)
	secret := "sk-" + strings.Repeat("Q", 30)
	encoded := base64.StdEncoding.EncodeToString([]byte(secret))
	url := "%73%6b%2d" + strings.Repeat("%51", 30)
	first := base64.StdEncoding.EncodeToString([]byte("-----BEGIN " + strings.Repeat(" ", 20)))
	second := base64.StdEncoding.EncodeToString([]byte("RSA PRIVATE KEY-----" + strings.Repeat(" ", 12)))
	bfSecret := credentialFixtures()["sk_bf_key"]
	for _, text := range []string{
		url,
		encoded,
		"%73%6b%2d%62%66%2d" + bfSecret[len("sk-bf-"):],
		base64.StdEncoding.EncodeToString([]byte(bfSecret)),
		" \t" + encoded + "\t \r\n",
		encoded[:8] + "\r" + encoded[8:],
		base64.URLEncoding.EncodeToString([]byte("😺 " + secret)),
		base64.StdEncoding.EncodeToString([]byte(url)),
		first + "\n非编码行\n" + second + "\n",
		url + "\n" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("普通安全文字", 10))),
	} {
		t.Run(fmt.Sprintf("%x", text[:min(len(text), 12)]), func(t *testing.T) {
			whole, _ := engine.Check(t.Context(), text, DirectionResponse, "")
			if !whole.Blocked {
				t.Fatal("完整检查未识别编码凭据")
			}
			for step := 1; step <= 7; step++ {
				s := credentialSession(t, engine)
				runes := []rune(text)
				blocked, release := false, 0
				for start := 0; start < len(runes); start += step {
					end := min(start+step, len(runes))
					v, err := s.Push(t.Context(), StreamFragment{Text: string(runes[start:end]), Final: end == len(runes)})
					if err != nil {
						t.Fatal(err)
					}
					if v.Result != nil && v.Result.Blocked {
						blocked = true
						break
					}
					if v.ReleaseThrough < release || v.ReleaseThrough > len(text)-len(strings.TrimLeftFunc(text, unicode.IsSpace)) {
						t.Fatalf("编码候选提前放行：%+v", v)
					}
					release = v.ReleaseThrough
				}
				if !blocked {
					t.Fatalf("step=%d: 编码凭据漏拦", step)
				}
			}
		})
	}
	for _, text := range []string{encoded + "!", "无效 %5 %zz %", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("安全", 20))), "PASSWORD=abcdefgh12345678"} {
		s := credentialSession(t, engine)
		for i, r := range []rune(text) {
			v, err := s.Push(t.Context(), StreamFragment{Text: string(r), Final: i == len([]rune(text))-1})
			if err != nil || v.Result != nil {
				t.Fatalf("安全或无效编码被误拦：%+v %v", v, err)
			}
			if i == len([]rune(text))-1 && v.ReleaseThrough != len(text) {
				t.Fatal("完成时未释放安全候选")
			}
		}
	}
}

func TestIncrementalCredentialIsolationCategoriesAndFallback(t *testing.T) {
	engine := credentialEngine(t, `{"categories":["api_key"]}`)
	s := credentialSession(t, engine)
	_, _ = s.Push(t.Context(), StreamFragment{Key: "one", Text: "sk-" + strings.Repeat("Q", 10)})
	for _, key := range []string{"two", "three"} {
		v, err := s.Push(t.Context(), StreamFragment{Key: key, Text: strings.Repeat("Q", 10), Final: true})
		if err != nil || v.Result != nil {
			t.Fatalf("字段状态串用：%+v %v", v, err)
		}
	}
	other := credentialSession(t, engine)
	v, err := other.Push(t.Context(), StreamFragment{Text: "mysql://alpha:pw@host", Final: true})
	if err != nil || v.Result != nil {
		t.Fatal("类别筛选失效")
	}
	custom := credentialEngine(t, `{"custom_patterns":[{"name":"custom","regex":"CUSTOM.*END"}]}`)
	if _, err := custom.(StreamingEngine).NewStream(t.Context(), CheckRequest{}); !errors.Is(err, ErrStreamUnsupported) {
		t.Fatalf("任意自定义正则应严格回退：%v", err)
	}
	for _, cfg := range []string{`{}`, `{"custom_patterns":[{"name":"custom","regex":"CUSTOM.*END"}]}`} {
		svc := incrementalService()
		svc.rules[0].Type = "credential_detection"
		svc.rules[0].Config = datatypes.JSON(cfg)
		svc.rules[0].Action = "mask"
		r, err := svc.NewResponseStream(t.Context(), CheckRequest{Model: "test"})
		if err != nil || !r.Fallback {
			t.Fatal("凭据脱敏策略未严格回退", err)
		}
		r.Close()
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Push(ctx, StreamFragment{Text: "a"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s.Close()
	if _, err := s.Push(t.Context(), StreamFragment{Text: "a"}); err == nil {
		t.Fatal("已关闭会话仍接收片段")
	}
}

func FuzzIncrementalCredentialMatchesComplete(f *testing.F) {
	for _, secret := range credentialFixtures() {
		f.Add("普通文字："+secret+"，结束。", uint8(1))
		f.Add(base64.StdEncoding.EncodeToString([]byte(secret)), uint8(3))
	}
	f.Add("Bearer "+strings.Repeat("Q", 24)+"example ", uint8(4))
	f.Add("普通文字 %5 %zz %", uint8(2))
	f.Add("PASSWORD="+strings.Repeat("Q", 12)+"\n普通：sk-"+strings.Repeat("Q", 20), uint8(3))
	f.Fuzz(func(t *testing.T, text string, size uint8) {
		if len(text) > 2048 || !utf8.ValidString(text) {
			t.Skip()
		}
		engine := credentialEngine(t, `{}`)
		whole, _ := engine.Check(t.Context(), text, DirectionResponse, "")
		s := credentialSession(t, engine)
		runes, step := []rune(text), int(size)%16+1
		blocked, release, total := false, 0, 0
		for start := 0; start <= len(runes); start += step {
			end := min(start+step, len(runes))
			fragment := string(runes[start:end])
			total += len(fragment)
			v, err := s.Push(t.Context(), StreamFragment{Text: fragment, Final: end == len(runes)})
			if err != nil {
				t.Fatal(err)
			}
			if v.Result != nil && v.Result.Blocked {
				blocked = true
				break
			}
			if v.ReleaseThrough < release || v.ReleaseThrough > total {
				t.Fatalf("放行水位无效：%+v", v)
			}
			if whole.Blocked {
				for _, pattern := range engine.(*credentialDetectionEngine).activePatterns {
					if pattern.Confidence == "low" {
						continue
					}
					for _, loc := range pattern.re.FindAllStringIndex(text, -1) {
						if pattern.Confidence == "medium" && !validateMediumConfidenceMatch(text[loc[0]:loc[1]]) {
							continue
						}
						if v.ReleaseThrough > loc[0] {
							t.Fatalf("凭据候选泄露：%+v pattern=%s start=%d", v, pattern.Name, loc[0])
						}
					}
				}
			}
			release = v.ReleaseThrough
			if end == len(runes) {
				break
			}
		}
		if blocked != whole.Blocked {
			t.Fatalf("增量=%v 完整=%v，text=%q", blocked, whole.Blocked, text)
		}
	})
}

func BenchmarkIncrementalCredentialSafe(b *testing.B) {
	s := credentialSession(b, credentialEngine(b, `{}`))
	b.ReportAllocs()
	for range b.N {
		if _, err := s.Push(context.Background(), StreamFragment{Text: "普通文字持续输出。\n"}); err != nil {
			b.Fatal(err)
		}
	}
}
