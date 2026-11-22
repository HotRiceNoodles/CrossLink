package admin

import (
	"github.com/crosslink/internal/guardrail"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSharedProfilePublication(t *testing.T) {
	env := newM3AEnv(t, false, 7)
	require.NoError(t, env.db.AutoMigrate(&guardrail.SecurityProfile{}, &guardrail.SecurityProfileVersion{}, &guardrail.SecurityProfileBinding{}))
	require.NoError(t, env.db.Exec("CREATE TABLE api_keys (id integer,org_id integer,team_id integer,created_by_id integer,expires_at datetime,status integer,deleted_at datetime)").Error)
	require.NoError(t, env.db.Exec("INSERT INTO api_keys(id,org_id,status) VALUES (11,7,1),(12,7,1),(13,8,1)").Error)
	group := env.router.Group("", func(c *gin.Context) { c.Set("role_name", "admin") })
	next := func(c *gin.Context) { c.Next() }
	NewGuardrailHandler(env.db, env.svcA, nil).RegisterPromptSecurity(group, next, next, next)
	req := func(method, path string, body any, code int) {
		w := env.req(t, method, "/prompt-security"+path, body)
		require.Equal(t, code, w.Code, w.Body.String())
	}
	req("POST", "/profiles", map[string]any{"name": "客服助手", "purpose": "查询订单"}, 201)
	req("PUT", "/bindings", map[string]any{"profile_id": 1, "api_key_ids": []int{11, 12}}, 400)
	req("POST", "/profiles/1/publish", map[string]int{"revision": 1}, 200)
	req("PUT", "/bindings", map[string]any{"profile_id": 1, "api_key_ids": []int{11, 12}}, 204)
	req("PUT", "/bindings", map[string]any{"profile_id": 0, "api_key_ids": []int{11, 13}}, 400)
	req("PUT", "/profiles/1", map[string]any{"revision": 2, "name": "客服助手", "purpose": "解释售后政策"}, 200)
	req("POST", "/profiles/1/publish", map[string]int{"revision": 2}, 409)
	req("POST", "/profiles/1/publish", map[string]int{"revision": 3}, 200)
	var versions []guardrail.SecurityProfileVersion
	require.NoError(t, env.db.Order("version").Find(&versions).Error)
	require.Len(t, versions, 2)
	require.Equal(t, "查询订单", versions[0].Purpose)
	require.Equal(t, int64(2), versions[1].Version)
	var count int64
	require.NoError(t, env.db.Model(&guardrail.SecurityProfileBinding{}).Count(&count).Error)
	require.EqualValues(t, 2, count)
	list := env.req(t, "GET", "/prompt-security/profiles", nil)
	require.Equal(t, 200, list.Code)
	require.Contains(t, list.Body.String(), `"bound_key_count":2`)
	// 列表带最近发布时间（取最新版本 created_at），供「应用场景」页按时间排序与展示
	require.Contains(t, list.Body.String(), `"updated_at"`)
	require.NotContains(t, list.Body.String(), `"updated_at":null`)

	// 仍被 Key 绑定的场景不允许删除：否则会静默清空 Key 的应用背景
	req("DELETE", "/profiles/1", nil, 409)
	req("PUT", "/bindings", map[string]any{"profile_id": 0, "api_key_ids": []int{11, 12}}, 204)
	req("DELETE", "/profiles/1", nil, 204)
	var left int64
	require.NoError(t, env.db.Model(&guardrail.SecurityProfile{}).Count(&left).Error)
	require.EqualValues(t, 0, left)
	require.NoError(t, env.db.Model(&guardrail.SecurityProfileVersion{}).Count(&left).Error)
	require.EqualValues(t, 0, left)
}
