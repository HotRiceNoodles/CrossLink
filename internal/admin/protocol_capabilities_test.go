package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// Phase-2 capability mirror: the endpoint reflects the code-level admission
// tables (sorted, complete) — the frontend card must never show a stale list.
func TestGatewayCapabilitiesHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	GatewayCapabilitiesHandler()(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var body struct {
		Data struct {
			BetaTokens              []string `json:"beta_tokens"`
			DialectParams           []string `json:"dialect_params"`
			AnthropicEffortVersions []string `json:"anthropic_effort_versions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	has := func(list []string, want string) bool {
		for _, v := range list {
			if v == want {
				return true
			}
		}
		return false
	}
	if !has(body.Data.BetaTokens, "compact-2026-01-12") || !has(body.Data.BetaTokens, "thinking-display-updates-2026-08-18") {
		t.Fatalf("beta_tokens = %v", body.Data.BetaTokens)
	}
	if !has(body.Data.DialectParams, "output_config") || !has(body.Data.DialectParams, "enable_thinking") {
		t.Fatalf("dialect_params = %v", body.Data.DialectParams)
	}
	if !has(body.Data.AnthropicEffortVersions, "2026-10-21") {
		t.Fatalf("effort versions = %v", body.Data.AnthropicEffortVersions)
	}
}
