package middleware

// auth_org_disable_test.go —— 租户停用强制点（网关 key 路径）：
// key 所属 org status != 1 → 403 organization is disabled；
// org 查不到（检查器返回 nil）→ 放行（与限流中间件同策略：查询失败≠停用）；
// 无 org 的 key / 检查器未注入（nil）→ 行为与旧版一致。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
)

type fakeOrgStatus struct {
	m map[int64]*model.Organization
}

func (f *fakeOrgStatus) Get(_ context.Context, orgID int64) *model.Organization {
	return f.m[orgID]
}

func TestAuthOrgDisabledDeniesGatewayKey(t *testing.T) {
	orgID := int64(7)
	validKey := &model.APIKey{
		ID:        42,
		KeyPrefix: "sk-test",
		Status:    1,
		OrgID:     &orgID,
	}
	keySvc, _ := newKeyServiceWithCachedKey(t, "secret-key-123", validKey)

	status := &fakeOrgStatus{m: map[int64]*model.Organization{
		orgID: {ID: orgID, Status: 0}, // 停用
	}}
	r := setupRouter()
	r.Use(Auth("", keySvc, nil, service.NoopPolicy{}, status))
	r.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("x-api-key", "secret-key-123")
	req.RemoteAddr = "1.2.3.4:5678"
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (org disabled), got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "organization is disabled") {
		t.Errorf("expected org-disabled message, got: %s", w.Body.String())
	}
}

func TestAuthOrgActiveOrUnknownPasses(t *testing.T) {
	orgID := int64(7)
	makeKey := func() *model.APIKey {
		return &model.APIKey{ID: 42, KeyPrefix: "sk-test", Status: 1, OrgID: &orgID}
	}

	cases := []struct {
		name string
		org  *model.Organization
	}{
		{"active org passes", &model.Organization{ID: orgID, Status: 1}},
		{"org lookup miss passes (nil = 查询失败≠停用)", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keySvc, _ := newKeyServiceWithCachedKey(t, "secret-key-123", makeKey())
			r := setupRouter()
			r.Use(Auth("", keySvc, nil, service.NoopPolicy{}, &fakeOrgStatus{m: map[int64]*model.Organization{orgID: tc.org}}))
			r.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/test", nil)
			req.Header.Set("x-api-key", "secret-key-123")
			req.RemoteAddr = "1.2.3.4:5678"
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestAuthNilCheckerKeepsLegacyBehavior(t *testing.T) {
	/* nil 检查器 = 未启用停用强制（兼容既有部署/测试装配）：停用语义只由注入方决定 */
	orgID := int64(7)
	key := &model.APIKey{ID: 42, KeyPrefix: "sk-test", Status: 1, OrgID: &orgID}
	keySvc, _ := newKeyServiceWithCachedKey(t, "secret-key-123", key)

	r := setupRouter()
	r.Use(Auth("", keySvc, nil, service.NoopPolicy{}, nil))
	r.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("x-api-key", "secret-key-123")
	req.RemoteAddr = "1.2.3.4:5678"
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("nil checker must not change behavior, got %d", w.Code)
	}
}
