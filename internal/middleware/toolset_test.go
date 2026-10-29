package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestParseDeclaredToolsFromRaw(t *testing.T) {
	anthropicBody := []byte(`{"model":"claude","tools":[
		{"name":"read_file","description":"read a file","input_schema":{"type":"object"}},
		{"description":"nameless, skipped"}
	]}`)
	declared := parseDeclaredToolsFromRaw("anthropic", anthropicBody)
	if len(declared) != 1 || declared[0].Name != "read_file" || declared[0].Description != "read a file" {
		t.Fatalf("anthropic parse = %+v", declared)
	}
	if string(declared[0].SchemaRaw) != `{"type":"object"}` {
		t.Fatalf("anthropic schema = %s", declared[0].SchemaRaw)
	}

	chatBody := []byte(`{"tools":[{"type":"function","function":{"name":"exec_shell","description":"sh"}}]}`)
	declared = parseDeclaredToolsFromRaw("openai", chatBody)
	if len(declared) != 1 || declared[0].Name != "exec_shell" || declared[0].Description != "sh" {
		t.Fatalf("openai nested parse = %+v", declared)
	}

	responsesBody := []byte(`{"tools":[
		{"type":"function","name":"flat_tool","parameters":{"type":"object"}},
		{"type":"web_search_preview"}
	]}`)
	declared = parseDeclaredToolsFromRaw("openai", responsesBody)
	if len(declared) != 1 || declared[0].Name != "flat_tool" {
		t.Fatalf("openai flat parse = %+v", declared)
	}

	for name, body := range map[string][]byte{
		"malformed": []byte(`{oops`),
		"no-tools":  []byte(`{"messages":[]}`),
	} {
		if got := parseDeclaredToolsFromRaw("openai", body); got != nil {
			t.Fatalf("%s: expected nil, got %+v", name, got)
		}
	}

	var tools string
	for i := 0; i < maxBlockedDeclaredTools+5; i++ {
		if i > 0 {
			tools += ","
		}
		tools += `{"type":"function","function":{"name":"t` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `"}}`
	}
	if got := parseDeclaredToolsFromRaw("openai", []byte(`{"tools":[`+tools+`]}`)); len(got) != maxBlockedDeclaredTools {
		t.Fatalf("cap not enforced: %d", len(got))
	}
}

type recordingToolObserver struct{ obs []domain.ToolObservation }

func (r *recordingToolObserver) Observe(o domain.ToolObservation) { r.obs = append(r.obs, o) }

// newBlockedGuardrailSvc builds an enabled guardrail service with one
// request-direction keyword block rule on BLOCKWORD.
func newBlockedGuardrailSvc(t *testing.T) *guardrail.GuardrailService {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { sqlDB.Close() })
	for _, stmt := range []string{
		"CREATE TABLE system_settings (key TEXT, value TEXT)",
		"INSERT INTO system_settings VALUES ('guardrails_enabled','true')",
		"CREATE TABLE guardrail_rules (id INTEGER PRIMARY KEY, name TEXT, type TEXT, direction TEXT, enabled BOOLEAN, config TEXT, severity TEXT, action TEXT)",
		`INSERT INTO guardrail_rules VALUES (1,'blocker','keyword_filter','request',true,'{"blocklist":["BLOCKWORD"]}','high','block')`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	gs := guardrail.NewGuardrailService(db, nil)
	gs.RefreshSettings(t.Context())
	return gs
}

func postBlocked(t *testing.T, r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("fixture rule did not block (%d)", w.Code)
	}
	return w
}

func TestGuardrailsRequestBlockedObservesDeclaredTools(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := &recordingToolObserver{}
	r := gin.New()
	teamID := int64(5)
	r.Use(func(c *gin.Context) {
		c.Set("api_key", &model.APIKey{ID: 42, TeamID: &teamID})
		c.Set("org_id", int64(7))
	})
	r.Use(GuardrailsRequest(newBlockedGuardrailSvc(t), rec))
	r.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusOK) })

	body := `{"model":"m","messages":[{"role":"user","content":"run BLOCKWORD now"}],
		"tools":[{"type":"function","function":{"name":"exec_shell","description":"sh","parameters":{"type":"object"}}}]}`
	postBlocked(t, r, "/v1/chat/completions", body)

	if len(rec.obs) != 1 {
		t.Fatalf("expected 1 observation, got %d", len(rec.obs))
	}
	o := rec.obs[0]
	if o.APIKeyID != 42 || o.TeamID != 5 || o.OrgID != 7 {
		t.Fatalf("identity = %+v", o)
	}
	if o.Protocol != "openai" || o.Source != "proxy" || o.Model != "m" {
		t.Fatalf("meta = %+v", o)
	}
	if len(o.Declared) != 1 || o.Declared[0].Name != "exec_shell" {
		t.Fatalf("declared = %+v", o.Declared)
	}
	if len(o.Invoked) != 0 {
		t.Fatalf("blocked path must not report invocations, got %v", o.Invoked)
	}
}

func TestGuardrailsRequestBlockedAnthropicProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := &recordingToolObserver{}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("api_key", &model.APIKey{ID: 42}) })
	r.Use(GuardrailsRequest(newBlockedGuardrailSvc(t), rec))
	r.POST("/v1/messages", func(c *gin.Context) { c.Status(http.StatusOK) })

	body := `{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":"BLOCKWORD"}],
		"tools":[{"name":"read_file","description":"read","input_schema":{"type":"object"}}]}`
	postBlocked(t, r, "/v1/messages", body)

	if len(rec.obs) != 1 || rec.obs[0].Protocol != "anthropic" {
		t.Fatalf("observations = %+v", rec.obs)
	}
	if len(rec.obs[0].Declared) != 1 || rec.obs[0].Declared[0].Name != "read_file" {
		t.Fatalf("declared = %+v", rec.obs[0].Declared)
	}
}

func TestGuardrailsRequestBlockedNilObserverAndKeylessNoOp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newBlockedGuardrailSvc(t)
	body := `{"model":"m","messages":[{"role":"user","content":"BLOCKWORD"}],"tools":[{"type":"function","function":{"name":"exec_shell"}}]}`

	// Community: nil observer must be inert.
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("api_key", &model.APIKey{ID: 42}) })
	r.Use(GuardrailsRequest(svc, nil))
	r.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusOK) })
	postBlocked(t, r, "/v1/chat/completions", body)

	// Keyless caller (e.g. playground): observation dropped, block unaffected.
	rec := &recordingToolObserver{}
	r2 := gin.New()
	r2.Use(GuardrailsRequest(svc, rec))
	r2.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusOK) })
	postBlocked(t, r2, "/v1/chat/completions", body)
	if len(rec.obs) != 0 {
		t.Fatalf("keyless request must not be observed, got %+v", rec.obs)
	}
}
