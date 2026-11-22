package admin

import (
	"context"
	"github.com/crosslink/internal/guardrail"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestScopedRuleLifecycle(t *testing.T) {
	env := newM3AEnv(t, false, 7)
	require.NoError(t, env.db.Exec("CREATE TABLE api_keys (id integer, org_id integer, team_id integer, created_by_id integer, expires_at datetime, status integer, deleted_at datetime)").Error)
	require.NoError(t, env.db.Exec("CREATE TABLE team_members (user_id integer, team_id integer, deleted_at datetime)").Error)
	require.NoError(t, env.db.Exec("INSERT INTO api_keys (id,org_id,created_by_id,status) VALUES (11,7,7,1),(12,7,9,1),(13,8,7,1)").Error)
	payload := map[string]any{"name": "指定 Key", "type": "keyword_filter", "direction": "both", "config": map[string]any{"blocklist": []string{"danger"}}, "scope_mode": "api_keys"}
	for _, ids := range [][]int64{{}, {12}, {13}, {11, 11}} {
		payload["api_key_ids"] = ids
		require.Equal(t, 400, env.req(t, "POST", "/guardrails", payload).Code)
	}
	payload["api_key_ids"] = []int64{11}
	w := env.req(t, "POST", "/guardrails", payload)
	require.Equal(t, 201, w.Code, w.Body.String())
	for _, id := range []int64{11, 12, 0} {
		result, err := env.svcA.Check(context.Background(), &guardrail.CheckRequest{Content: "danger", OrgID: 7, APIKeyID: id, Direction: guardrail.DirectionRequest})
		require.NoError(t, err)
		require.Equal(t, id == 11, result.Blocked)
	}
	require.Equal(t, 200, env.req(t, "PUT", "/guardrails/1", map[string]any{"name": "更新"}).Code)
	w = env.req(t, "POST", "/guardrails/1/copy", map[string]string{"name": "客服副本"})
	require.Equal(t, 201, w.Code, w.Body.String())
	var duplicate guardrail.GuardrailRule
	require.NoError(t, env.db.First(&duplicate, 2).Error)
	require.False(t, duplicate.Enabled)
	require.Equal(t, "api_keys", duplicate.ScopeMode)
	require.Equal(t, []int64{11}, []int64(duplicate.APIKeyIDs))
	require.Equal(t, 200, env.req(t, "PUT", "/guardrails/2", map[string]any{"scope_mode": "all", "api_key_ids": []int{}}).Code)
	require.Equal(t, 200, env.req(t, "POST", "/guardrails/2/rollback", map[string]int{"to_version": 1}).Code)
	require.Equal(t, 409, env.req(t, "POST", "/guardrails/2/rollback", map[string]int{"to_version": 2}).Code)
	require.Equal(t, 200, env.req(t, "POST", "/guardrails/2/rollback", map[string]any{"to_version": 2, "expected_version": 3, "confirm_wider_scope": true}).Code)
	require.NoError(t, env.db.Exec("DELETE FROM role_permissions").Error)
	require.Equal(t, 400, env.req(t, "POST", "/guardrails/1/copy", map[string]string{"name": "无权复制"}).Code)
	require.NoError(t, env.db.Exec("INSERT INTO role_permissions VALUES(7,'key:update')").Error)
	require.NoError(t, env.db.Exec("UPDATE api_keys SET status=0 WHERE id=11").Error)
	require.Equal(t, 400, env.req(t, "POST", "/guardrails/1/rollback", map[string]any{"to_version": 1}).Code)
	var saved guardrail.GuardrailRule
	require.NoError(t, env.db.First(&saved).Error)
	require.Equal(t, "api_keys", saved.ScopeMode)
	require.Equal(t, []int64{11}, []int64(saved.APIKeyIDs))
}
