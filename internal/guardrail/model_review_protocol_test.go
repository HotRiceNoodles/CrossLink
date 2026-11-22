package guardrail

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type reviewTransport func(*http.Request) (*http.Response, error)

func (f reviewTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// 内存传输验证协议边界；实际套接字与 SSRF 白名单由管理员到网关集成测试覆盖。
func TestReviewProtocolValidation(t *testing.T) {
	db := setupSeedDB(t)
	require.NoError(t, db.AutoMigrate(&ReviewService{}))
	// SQLite 的正式迁移使用 TEXT 时间列，读取审核连接时不依赖时间反序列化。
	require.NoError(t, db.Exec(`CREATE TABLE providers(id integer,org_id integer,status integer,adapter_type text,base_url text,api_key text,deleted_at text,created_at text);
CREATE TABLE provider_models(id integer,provider_id integer,provider_model text,status integer,deleted_at text,created_at text);
INSERT INTO providers VALUES(1,7,1,'openai_compatible','https://review.example/v1','fixture-token',NULL,'2026-11-18 00:00:00');
INSERT INTO provider_models VALUES(1,1,'teacher',1,NULL,'2026-11-18 00:00:00');`).Error)
	s := NewGuardrailService(db, nil)
	r := ReviewService{OrgID: 7, Name: "第三方审核", Kind: "provider", ProviderModelID: 1, TimeoutMS: 100}
	require.NoError(t, s.ConnectReview(t.Context(), &r, ""))
	local := ReviewService{OrgID: 7, Name: "本地审核", Kind: "local", ModelVersion: "student-B", Threshold: 0.7, TimeoutMS: 10, Endpoint: "http://10.0.0.8:18081"}
	require.NoError(t, db.Create(&local).Error)
	previous := reviewHTTP
	t.Cleanup(func() { reviewHTTP = previous })
	in := PlainDetectionInput("测试正文")
	profile := &SecurityProfileVersion{ProfileContent: ProfileContent{Name: "客服", Purpose: "查询订单"}}
	for _, tc := range []struct {
		name    string
		local   bool
		reply   string
		status  string
		verdict string
		score   bool
	}{
		{"第三方阻断", false, `{"model":"teacher-v1","choices":[{"finish_reason":"stop","message":{"content":"{\"verdict\":\"unsafe\"}"}}]}`, "", "unsafe", false},
		{"第三方放行", false, `{"model":"teacher-v1","choices":[{"finish_reason":"stop","message":{"content":"{\"verdict\":\"safe\"}"}}]}`, "", "safe", false},
		{"第三方缺上下文", false, `{"model":"teacher-v1","choices":[{"finish_reason":"stop","message":{"content":"{\"verdict\":\"missing_context\"}"}}]}`, "", "missing_context", false},
		{"第三方截断", false, `{"model":"teacher-v1","choices":[{"finish_reason":"length","message":{"content":"{\"verdict\":\"safe\"}"}}]}`, "", "invalid_output", false},
		{"本地分数", true, "score", "", "unsafe", true},
		{"本地非法分数", true, "bad-score", "", "invalid_output", false},
		{"本地覆盖错误", true, "bad-segments", "", "invalid_output", false},
		{"本地版本漂移", true, `{"version":"crosslink.injection-result.v1","status":"ok","coverage":"full","model_version":"student-C"}`, "", "version_mismatch", false},
		{"本地弃权", true, `{"version":"crosslink.injection-result.v1","status":"abstain","coverage":"partial","model_version":"student-B","injection_score":null}`, "", "partial", false},
		{"本地超时", true, "", "timeout", "timeout", false},
		{"本地不可用", true, "", "unavailable", "unavailable", false},
		{"本地无效输出", true, `{}`, "", "invalid_output", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reviewHTTP = &http.Client{Transport: reviewTransport(func(req *http.Request) (*http.Response, error) {
				raw, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				if tc.local {
					require.Equal(t, "10.0.0.8:18081", req.URL.Host)
					require.NotContains(t, string(raw), "查询订单")
				} else {
					require.Equal(t, "review.example", req.URL.Host)
					require.Equal(t, "Bearer fixture-token", req.Header.Get("Authorization"))
					require.Equal(t, "1", req.Header.Get("X-CrossLink-Model-Review"))
					require.Contains(t, string(raw), "查询订单")
					require.NotContains(t, string(raw), "profile_id")
				}
				if tc.status == "timeout" {
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				status := 200
				if tc.status == "unavailable" {
					status = 503
				}
				body := tc.reply
				if strings.HasSuffix(tc.reply, "score") || tc.reply == "bad-segments" {
					ids := []string{in.Segments[0].ID}
					score := 0.9
					if tc.reply == "bad-score" {
						score = 1.2
					}
					if tc.reply == "bad-segments" {
						ids = []string{"other"}
					}
					data, err := json.Marshal(map[string]any{"version": "crosslink.injection-result.v1", "status": "ok", "coverage": "full", "model_version": "student-B", "injection_score": score, "window_segments": ids})
					require.NoError(t, err)
					body = string(data)
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			id := r.ID
			if tc.local {
				id = local.ID
			}
			result := s.reviewOne(t.Context(), 7, ReviewConfig{ServiceID: id}, in, profile, nil)
			require.Equal(t, tc.verdict, result.Status)
			require.Equal(t, tc.score, result.Score != nil)
		})
	}
}
