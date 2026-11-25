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
	"github.com/crosslink/internal/license"
	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 档位 × 租户上下文矩阵：审核服务侧（review-services/catalog/keys）自 2026-11-24
// 下放 Pro——企业档 org=0 仍拒（必须选组织），社区/Pro 放行（社区档由
// RequireAction 的 guardrail:* 动作位拦截）；应用场景侧（profiles/bindings）
// 保持仅企业（org>0 无条件门，与前端 ROUTE_TIER['prompt-profiles'] 同口径）。
func TestPromptSecurityTierGates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tier    string
		orgID   int64
		review  int
		profile int
	}{
		{"community orgless", license.TierCommunity, 0, 200, 403},
		{"pro orgless", license.TierPro, 0, 200, 403},
		{"enterprise orgless", license.TierEnterprise, 0, 403, 403},
		{"enterprise org context", license.TierEnterprise, 7, 200, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTier(t, tc.tier)
			env := newM3AEnv(t, false, tc.orgID)
			require.NoError(t, env.db.AutoMigrate(&guardrail.SecurityProfile{}, &guardrail.SecurityProfileVersion{}, &guardrail.SecurityProfileBinding{}, &guardrail.ReviewService{}))
			group := env.router.Group("", func(c *gin.Context) { c.Set("role_name", "admin") })
			next := func(c *gin.Context) { c.Next() }
			NewGuardrailHandler(env.db, env.svcA, nil).RegisterPromptSecurity(group, next, next, next)
			w := env.req(t, "GET", "/prompt-security/review-services", nil)
			require.Equal(t, tc.review, w.Code, w.Body.String())
			w = env.req(t, "GET", "/prompt-security/profiles", nil)
			require.Equal(t, tc.profile, w.Code, w.Body.String())
		})
	}
}

// 单租户全链路（Pro 档 admin 上下文 org=0）：
//   ① 供应商目录 OR-0 谓词命中 org=NULL 供应商（admin 所建、未经 boot 回填）
//   ② 审核服务以 org=0 接入（ConnectReview 不再拒 OrgID=0）
//   ③ 无组织作用域规则（org=0 → OrgID=nil）引用 org-0 服务通过校验
//   ④ 运行时回填到 default org（org=1）的 Key 也能解析到 org-0 服务（providerCalls 佐证）
//   ⑤ 应用场景侧仍按企业口径 403
func TestPromptSecuritySingleTenant(t *testing.T) {
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
	var providerCalls atomic.Int32
	var mode atomic.Value
	mode.Store("safe")
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/chat/completions" {
			providerCalls.Add(1)
			json.NewEncoder(w).Encode(gin.H{"model": "teacher-rev1", "choices": []any{gin.H{"finish_reason": "stop", "message": gin.H{"content": fmt.Sprintf(`{"verdict":%q}`, mode.Load())}}}})
			return
		}
		http.NotFound(w, r)
	}))
	up.Listener.Close()
	up.Listener = listener
	up.Start()
	defer up.Close()

	withTier(t, license.TierPro)
	env := newM3AEnv(t, false, 0) // orgless：不设 org_id，模拟单租户 admin 上下文
	require.NoError(t, env.db.AutoMigrate(&model.APIKey{}, &model.APIKeyHash{}, &model.TeamMember{}, &model.Provider{}, &model.ProviderModel{}, &guardrail.ReviewService{}, &guardrail.SecurityProfile{}, &guardrail.SecurityProfileVersion{}, &guardrail.SecurityProfileBinding{}))
	// admin 所建供应商 org=NULL（providers.go：org=0 不赋值，boot 回填前的形态）
	prov := model.Provider{Name: "fixture", DisplayName: "fixture", AdapterType: "openai_compatible", BaseURL: up.URL + "/v1", APIKey: "fixture-only"}
	require.NoError(t, env.db.Create(&prov).Error)
	pm := model.ProviderModel{ProviderID: prov.ID, ModelName: "审核模型", ProviderModel: "teacher"}
	require.NoError(t, env.db.Create(&pm).Error)
	// key0：admin 所建（KeyService 对 org=0 落 NULL，boot 回填前形态）；
	// key1：boot 回填到 default org 的存量 Key（phases.go backfillOrgID 语义）
	ks := service.NewKeyService(repository.NewAPIKeyRepo(env.db), repository.NewAPIKeyHashRepo(env.db), env.db, &crypto.StandardProvider{}, nil)
	key0, err := ks.Create(t.Context(), &service.CreateKeyInput{Name: "客服", CreatedByID: 7})
	require.NoError(t, err)
	key1, err := ks.Create(t.Context(), &service.CreateKeyInput{Name: "存量", OrgID: 1, CreatedByID: 7})
	require.NoError(t, err)

	h := NewGuardrailHandler(env.db, env.svcA, nil)
	next := func(c *gin.Context) { c.Next() }
	h.RegisterPromptSecurity(env.router.Group("", func(c *gin.Context) { c.Set("role_name", "admin") }), next, next, next)
	admin := func(method, path string, body any, code int) {
		t.Helper()
		w := env.req(t, method, path, body)
		require.Equal(t, code, w.Code, w.Body.String())
	}

	w := env.req(t, "GET", "/prompt-security/catalog", nil)
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "teacher", "org=0 目录须命中 org=NULL 供应商")
	admin("POST", "/prompt-security/review-services", gin.H{"kind": "provider", "name": "教师", "provider_model_id": pm.ID}, 201)
	admin("GET", "/prompt-security/review-services", nil, 200)
	admin("GET", "/prompt-security/profiles", nil, 403)
	// 无组织作用域规则（org=0 → OrgID=nil）引用 org-0 服务：rule1 按 Key 限定
	// （setScope org=0 即无租户过滤），rule2 全量范围
	admin("POST", "/guardrails", gin.H{"name": "客服审核", "type": "model_review", "direction": "request", "action": "block", "scope_mode": "api_keys", "api_key_ids": []int64{key0.ID}, "config": gin.H{"service_id": 1}}, 201)
	admin("POST", "/guardrails", gin.H{"name": "全量审核", "type": "model_review", "direction": "request", "action": "block", "config": gin.H{"service_id": 1}}, 201)

	env.svcA.SetEnabled(true)
	gw := gin.New()
	gw.Use(middleware.Auth("", ks, nil, nil, nil, nil), middleware.OrgResolve(), middleware.GuardrailsRequest(env.svcA, nil))
	gw.POST("/v1/chat/completions", func(c *gin.Context) { c.String(200, "business response") })
	ask := func(key *service.CreateKeyResult, code int) {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"business","messages":[{"role":"user","content":"查询"}],"stream":false}`))
		req.Header.Set("Authorization", "Bearer "+key.APIKey)
		gw.ServeHTTP(w, req)
		require.Equal(t, code, w.Code, w.Body.String())
	}
	ask(key0, 200) // rule1+rule2 各审一次（safe 放行）
	require.EqualValues(t, 2, providerCalls.Load())
	ask(key1, 200) // rule2 审一次：org=1 Key 必须解析到 org=0 审核服务
	require.EqualValues(t, 3, providerCalls.Load())
	mode.Store("unsafe")
	ask(key0, 400) // rule1+rule2 各审一次，block
	ask(key1, 400) // rule2 命中 block
	require.EqualValues(t, 6, providerCalls.Load())
}
