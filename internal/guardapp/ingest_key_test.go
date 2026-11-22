package guardapp

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestIngestUsesCredentialKeyScope(t *testing.T) {
	r, _, db := newTestAdminAPIWithIngest(t)
	admin := adminSession(t, r)
	require.NoError(t, db.Exec("INSERT INTO api_keys (id,name,key_hash,key_prefix,org_id) VALUES(11,'客服','fixture','prefix',1)").Error)
	post := func(path, token, body string, code int) string {
		t.Helper()
		w := doJSON(t, r, "POST", path, token, body)
		require.Equal(t, code, w.Code, w.Body.String())
		return w.Body.String()
	}
	post("/admin/api/guardrails", admin, `{"name":"客服","type":"keyword_filter","direction":"both","scope_mode":"api_keys","api_key_ids":[11],"config":{"blocklist":["danger"]}}`, 201)
	token := func(body string) string {
		var v struct{ Data CreatedToken }
		require.NoError(t, json.Unmarshal([]byte(post("/admin/api/guard/tokens", admin, body, 201)), &v))
		return v.Data.Token
	}
	bound := token(`{"name":"关联","api_key_id":11}`)
	plain := token(`{"name":"未关联"}`)
	body := `{"trace_id":"same","protocol":"generic","api_key_id":11,"content":"danger","metadata":{"api_key_id":"11"}}`
	require.Contains(t, post("/api/v1/detect", plain, body, 200), `"verdict":"allow"`)
	require.Contains(t, post("/api/v1/detect", bound, body, 200), `"verdict":"block"`)
	require.NoError(t, db.Exec("UPDATE api_keys SET status=0 WHERE id=11").Error)
	post("/api/v1/detect", bound, body, 403)
}
