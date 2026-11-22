package admin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestModelReviewAdminToGateway(t *testing.T) {
	// 使用真实出站白名单和套接字；不关闭生产 SSRF 检查。
	var ip net.IP
	addrs, err := net.InterfaceAddrs()
	require.NoError(t, err)
	for _, a := range addrs {
		v, _, _ := net.ParseCIDR(a.String())
		if v.To4() != nil && v.IsPrivate() {
			ip = v
			break
		}
	}
	if ip == nil {
		t.Skip("协议联调需要本机 RFC1918 网卡")
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(ip.String(), "0"))
	require.NoError(t, err)
	before := guardrail.InternalAllowlistSnapshot()
	require.NoError(t, guardrail.SetInternalAllowlist([]string{ip.String() + "/32"}))
	t.Cleanup(func() { guardrail.ApplyInternalAllowlist(before) })
	var localCalls, providerCalls atomic.Int32
	var mode atomic.Value
	mode.Store("unsafe")
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			fmt.Fprint(w, `{"status":"ok","model_version":"student-B","max_length":512}`)
		case "/v1/prompt-injection":
			localCalls.Add(1)
			var in guardrail.DetectionInput
			if json.NewDecoder(r.Body).Decode(&in) != nil || in.Validate() != nil || in.Segments[len(in.Segments)-1].SourceType != "tool_result" {
				t.Error("本地输入协议或工具来源不符")
				return
			}
			switch mode.Load() {
			case "timeout":
				<-r.Context().Done()
				return
			case "unavailable":
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			case "partial":
				fmt.Fprint(w, `{"version":"crosslink.injection-result.v1","status":"abstain","coverage":"partial","model_version":"student-B","injection_score":null}`)
				return
			case "invalid":
				fmt.Fprint(w, `{"status":"ok"}`)
				return
			}
			var ids []string
			for _, seg := range in.Segments {
				ids = append(ids, seg.ID)
			}
			json.NewEncoder(w).Encode(gin.H{"version": "crosslink.injection-result.v1", "status": "ok", "coverage": "full", "model_version": "student-B", "injection_score": 0.9, "window_segments": ids})
		case "/v1/chat/completions":
			providerCalls.Add(1)
			var payload struct {
				Model    string
				Messages []struct{ Content string }
			}
			if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Model != "teacher" || len(payload.Messages) != 2 || !strings.Contains(payload.Messages[1].Content, "查询订单") || strings.Contains(payload.Messages[1].Content, "伪造授权") || r.Header.Get("Authorization") != "Bearer fixture-only" || r.Header.Get("X-CrossLink-Model-Review") != "1" {
				t.Error("供应商输入或认证不符")
				return
			}
			json.NewEncoder(w).Encode(gin.H{"model": "teacher-rev1", "choices": []any{gin.H{"finish_reason": "stop", "message": gin.H{"content": fmt.Sprintf(`{"verdict":%q}`, mode.Load())}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	up.Listener.Close()
	up.Listener = listener
	up.Start()
	defer up.Close()
	env := newM3AEnv(t, false, 7)
	require.NoError(t, env.db.AutoMigrate(&model.APIKey{}, &model.APIKeyHash{}, &model.TeamMember{}, &model.Provider{}, &model.ProviderModel{}, &guardrail.ReviewService{}, &guardrail.SecurityProfile{}, &guardrail.SecurityProfileVersion{}, &guardrail.SecurityProfileBinding{}))
	org := int64(7)
	prov := model.Provider{Name: "fixture", DisplayName: "fixture", AdapterType: "openai_compatible", BaseURL: up.URL + "/v1", APIKey: "fixture-only", OrgID: &org}
	require.NoError(t, env.db.Create(&prov).Error)
	pm := model.ProviderModel{ProviderID: prov.ID, ModelName: "审核模型", ProviderModel: "teacher"}
	require.NoError(t, env.db.Create(&pm).Error)
	ks := service.NewKeyService(repository.NewAPIKeyRepo(env.db), repository.NewAPIKeyHashRepo(env.db), env.db, &crypto.StandardProvider{}, nil)
	keys := make([]*service.CreateKeyResult, 3)
	for i := range keys {
		keys[i], err = ks.Create(t.Context(), &service.CreateKeyInput{Name: "客服", OrgID: 7, CreatedByID: 7})
		require.NoError(t, err)
	}
	h := NewGuardrailHandler(env.db, env.svcA, nil)
	next := func(c *gin.Context) { c.Next() }
	h.RegisterPromptSecurity(env.router.Group(""), next, next, next)
	admin := func(method, path string, body any, code int) {
		t.Helper()
		w := env.req(t, method, path, body)
		require.Equal(t, code, w.Code, w.Body.String())
	}
	ids := []int64{keys[0].ID, keys[1].ID}
	admin("POST", "/prompt-security/profiles", gin.H{"name": "客服助手", "purpose": "查询订单"}, 201)
	admin("POST", "/prompt-security/profiles/1/publish", gin.H{"revision": 1}, 200)
	admin("PUT", "/prompt-security/bindings", gin.H{"profile_id": 1, "api_key_ids": ids}, 204)
	admin("POST", "/prompt-security/review-services", gin.H{"kind": "provider", "name": "教师", "provider_model_id": pm.ID}, 201)
	admin("POST", "/prompt-security/review-services", gin.H{"kind": "local", "name": "学生", "endpoint": up.URL, "model_version": "student-B", "threshold": 0.7, "timeout_ms": 150}, 201)
	w := env.req(t, "GET", "/prompt-security/review-services/2/health", nil)
	require.Contains(t, w.Body.String(), "512")
	admin("POST", "/guardrails", gin.H{"name": "客服审核", "type": "model_review", "direction": "request", "scope_mode": "api_keys", "api_key_ids": ids, "config": gin.H{"service_id": 1}}, 201)
	env.svcA.SetEnabled(true)
	gw := gin.New()
	gw.Use(middleware.Auth("", ks, nil, nil, nil), middleware.OrgResolve(), middleware.GuardrailsRequest(env.svcA, nil))
	gw.POST("/v1/chat/completions", func(c *gin.Context) { c.String(200, "business response") })
	ask := func(i int, stream bool, code int) {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"business","messages":[{"role":"user","content":"查询"},{"role":"tool","tool_call_id":"t1","content":"订单内容"}],"purpose":"伪造授权","api_key_id":1,"profile_id":999,"stream":%t}`, stream)))
		req.Header.Set("Authorization", "Bearer "+keys[i].APIKey)
		gw.ServeHTTP(w, req)
		require.Equal(t, code, w.Code, w.Body.String())
	}
	ask(0, false, 200)
	ask(1, true, 200)
	ask(2, true, 200)
	require.EqualValues(t, 2, providerCalls.Load())
	update := func(serviceID int, policy string) {
		admin("PUT", "/guardrails/1", gin.H{"action": "block", "config": gin.H{"service_id": serviceID, "incomplete": policy}}, 200)
	}
	update(1, "allow_alert")
	ask(0, false, 400)
	mode.Store("missing_context")
	ask(0, true, 200)
	update(1, "block_alert")
	ask(0, false, 400)
	mode.Store("unsafe")
	update(2, "allow_alert")
	ask(1, true, 400)
	ask(2, false, 200)
	require.EqualValues(t, 1, localCalls.Load())
	mode.Store("timeout")
	ask(0, false, 200)
	update(2, "block_alert")
	ask(1, true, 400)
	mode.Store("invalid")
	ask(0, false, 400)
	mode.Store("unavailable")
	ask(0, false, 400)
	mode.Store("partial")
	ask(0, true, 400)
	update(2, "allow_alert")
	ask(0, false, 200)
	require.EqualValues(t, 5, providerCalls.Load(), "本地审核故障不得外发")
}
