package admin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// pgRespGuardProvider serves the 3B translation path: non-stream Chat returns
// tool arguments whose \u0043 escape decodes to "SECRET"; streams emit a
// marker split across chunks.
type pgRespGuardProvider struct {
	name  string
	calls int
}

func (p *pgRespGuardProvider) Chat(context.Context, *domain.OpenAIRequest, string) (*domain.OpenAIResponse, error) {
	p.calls++
	return &domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{{
			Message: domain.OpenAIMessage{
				Role: "assistant",
				ToolCalls: []domain.OpenAIToolCall{{
					ID: "c1", Type: "function",
					Function: domain.OpenAIFunctionCall{Name: "send", Arguments: `{"value":"SE\u0043RET"}`},
				}},
			},
		}},
	}, nil
}

func (p *pgRespGuardProvider) StreamChat(_ context.Context, _ *domain.OpenAIRequest, _ string) (<-chan domain.SSEChunk, error) {
	ch := make(chan domain.SSEChunk, 4)
	ch <- domain.SSEChunk{Chunk: &domain.OpenAIChunk{Choices: []domain.OpenAIChunkChoice{{Delta: domain.OpenAIChunkDelta{Content: "SE"}}}}}
	ch <- domain.SSEChunk{Chunk: &domain.OpenAIChunk{Choices: []domain.OpenAIChunkChoice{{Delta: domain.OpenAIChunkDelta{Content: "CRET"}}}}}
	ch <- domain.SSEChunk{Done: true}
	close(ch)
	return ch, nil
}

func (p *pgRespGuardProvider) Name() string { return p.name }

func pgRespGuardSetup(t *testing.T, streamMode string) (*gorm.DB, *guardrail.GuardrailService) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.AutoMigrate(&model.UsageLog{}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"CREATE TABLE system_settings (key TEXT, value TEXT)",
		"INSERT INTO system_settings VALUES ('guardrails_enabled','true'),('guardrails_stream_mode','" + streamMode + "')",
		"CREATE TABLE guardrail_rules (id INTEGER PRIMARY KEY, name TEXT, type TEXT, direction TEXT, enabled BOOLEAN, config TEXT, severity TEXT, action TEXT)",
		`INSERT INTO guardrail_rules VALUES (1,'pgresp','keyword_filter','response',true,'{"blocklist":["SECRET"]}','high','block')`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	gs := guardrail.NewGuardrailService(db, nil)
	gs.RefreshSettings(t.Context())
	return db, gs
}

func pgRespGuardHandler(t *testing.T, db *gorm.DB, gs *guardrail.GuardrailService) *PlaygroundHandler {
	t.Helper()
	reg := provider.NewRegistry()
	reg.Register("fake", &pgRespGuardProvider{name: "fake"})
	resolver := router.NewResolver(reg, &pgModelRepo{}, nil, map[router.StrategyName]router.RoutingStrategy{
		router.StrategyWeightedRandom: &router.WeightedRandomStrategy{},
	}, nil, nil, nil, nil)
	return NewPlaygroundHandler(resolver, service.NewUsageService(repository.NewUsageLogRepo(db)), nil, nil, nil, gs, nil)
}

func pgRespGuardRequest(t *testing.T, h *PlaygroundHandler, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"model":"test-model","input":"hi"}`
	if stream {
		body = `{"model":"test-model","input":"hi","stream":true}`
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/api/playground/responses", strings.NewReader(body))
	c.Set("org_id", int64(7))
	h.Responses(c)
	return w
}

// Non-stream 3B: tool arguments arriving as {"value":"SE\u0043RET"} decode to
// SECRET for the tool; the playground Responses output check must block on the
// decoded form (round-10 class: escaped JSON bypass).
func TestCodexRetestPlaygroundResponsesNonStreamBlocksDecodedToolArgs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, gs := pgRespGuardSetup(t, "window")
	h := pgRespGuardHandler(t, db, gs)
	w := pgRespGuardRequest(t, h, false)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "guardrail_blocked") {
		t.Fatalf("decoded tool arguments bypassed playground responses block: %d %s", w.Code, w.Body.String())
	}
}

// Strict stream: the split "SE"/"CRET" content must NOT be released before
// the final check — the recorder receives only the guardrail error.
func TestCodexRetestPlaygroundResponsesStrictStreamWithholdsBlockedContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, gs := pgRespGuardSetup(t, "strict")
	h := pgRespGuardHandler(t, db, gs)
	w := pgRespGuardRequest(t, h, true)
	body := w.Body.String()
	if strings.Contains(body, `"delta":"SE"`) || strings.Contains(body, `"delta":"CRET"`) {
		t.Fatalf("strict playground responses stream released unchecked content: %s", body)
	}
	if !strings.Contains(body, "guardrail_blocked") {
		t.Fatalf("strict playground responses stream did not block: %s", body)
	}
}

// Window stream: already-emitted content is not retractable (documented
// tradeoff) but the hit still terminates the stream with a guardrail error.
func TestCodexRetestPlaygroundResponsesWindowStreamTerminatesOnHit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, gs := pgRespGuardSetup(t, "window")
	h := pgRespGuardHandler(t, db, gs)
	w := pgRespGuardRequest(t, h, true)
	body := w.Body.String()
	if !strings.Contains(body, "guardrail_blocked") {
		t.Fatalf("window stream hit produced no guardrail error event: %s", body)
	}
}

func TestCodexRetestPgExtractResponsesTextDecodesEscapedLeaves(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"escaped arguments decode", `{"type":"response.function_call_arguments.done","arguments":"{\"value\":\"SE\u0043RET\"}"}`, `{"value":"SECRET"}`},
		{"delta verbatim", `{"type":"response.output_text.delta","delta":"SE"}`, "SE"},
		{"completed output walk", `{"output":[{"type":"message","content":[{"type":"output_text","text":"CRE"}]},{"type":"function_call","arguments":"{\"v\":\"T\"}"}]}`, "CRE{\"v\":\"T\"}"},
		{"response.completed nested output", `{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"SECRET"}]}]}}`, "SECRET"},
		{"output_item.done item walk", `{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"SECRET"}]}}`, "SECRET"},
		{"output_item.done function args", `{"type":"response.output_item.done","item":{"type":"function_call","arguments":"{\"v\":\"SE\u0043RET\"}"}}`, `{"v":"SECRET"}`},
		{"output_text.done top-level text", `{"type":"response.output_text.done","text":"SECRET"}`, "SECRET"},
		{"content_part.done part text", `{"type":"response.content_part.done","part":{"type":"output_text","text":"SECRET"}}`, "SECRET"},
		{"reasoning summary text", `{"type":"response.output_item.done","item":{"type":"reasoning","summary":[{"type":"summary_text","text":"SECRET"}]}}`, "SECRET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pgExtractResponsesText([]byte(tc.payload)); got != tc.want {
				t.Fatalf("pgExtractResponsesText = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- native 3A path (real provider.ResponsesProvider + supports_responses
// ExtraConfig) and fail-closed behavior for every playground Responses
// surface (round-11 findings 1+2). ---

type pgRespNativeProvider struct {
	name     string
	sse      string
	body     string
	calls    int
	cancelFn context.CancelFunc // when set, cancels the request ctx mid-call
}

func (p *pgRespNativeProvider) Name() string { return p.name }
func (p *pgRespNativeProvider) Chat(context.Context, *domain.OpenAIRequest, string) (*domain.OpenAIResponse, error) {
	return nil, fmt.Errorf("native provider does not serve chat")
}
func (p *pgRespNativeProvider) StreamChat(context.Context, *domain.OpenAIRequest, string) (<-chan domain.SSEChunk, error) {
	return nil, fmt.Errorf("native provider does not serve stream")
}
func (p *pgRespNativeProvider) Responses(ctx context.Context, _ []byte, _ string) (io.ReadCloser, int, error) {
	p.calls++
	if p.cancelFn != nil {
		p.cancelFn()
	}
	if p.sse != "" {
		return io.NopCloser(strings.NewReader(p.sse)), 200, nil
	}
	return io.NopCloser(strings.NewReader(p.body)), 200, nil
}

// pgNativeModelRepo marks the model as 3A-capable (supports_responses flag).
type pgNativeModelRepo struct{}

func (m *pgNativeModelRepo) FindByModelName(_ context.Context, name string, _ int64) ([]model.ProviderModel, error) {
	if name != "native-model" {
		return nil, nil
	}
	return []model.ProviderModel{{
		ID: 2, ModelName: name, ProviderModel: "native-fake", Weight: 100, Priority: 1, Status: 1,
		Provider:    model.Provider{Name: "native", Status: 1},
		ExtraConfig: datatypes.JSON(`{"supports_responses":true}`),
	}}, nil
}

func pgNativeHandler(t *testing.T, db *gorm.DB, gs *guardrail.GuardrailService, p *pgRespNativeProvider) *PlaygroundHandler {
	t.Helper()
	reg := provider.NewRegistry()
	reg.Register("native", p)
	resolver := router.NewResolver(reg, &pgNativeModelRepo{}, nil, map[router.StrategyName]router.RoutingStrategy{
		router.StrategyWeightedRandom: &router.WeightedRandomStrategy{},
	}, nil, nil, nil, nil)
	return NewPlaygroundHandler(resolver, service.NewUsageService(repository.NewUsageLogRepo(db)), nil, nil, nil, gs, nil)
}

func pgNativeRequest(t *testing.T, h *PlaygroundHandler, stream bool, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"model":"native-model","input":"hi"}`
	if stream {
		body = `{"model":"native-model","input":"hi","stream":true}`
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/playground/responses", strings.NewReader(body))
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	c.Request = req
	c.Set("org_id", int64(7))
	h.Responses(c)
	return w
}

// pgFailingGuardrailSvc builds an enabled, fail-closed service whose detector
// can never load (closed DB, zero cached rules) — every Check errors. The
// response-budget timeout path fails through the identical grErr branch.
func pgFailingGuardrailSvc(t *testing.T, failOpen bool) *guardrail.GuardrailService {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	gs := guardrail.NewGuardrailService(db, nil)
	gs.SetEnabled(true)
	gs.SetStreamMode("strict")
	gs.SetFailOpen(failOpen)
	sqlDB, _ := db.DB()
	sqlDB.Close() // rules/settings can never load: deterministic detector failure
	t.Cleanup(func() { sqlDB.Close() })
	return gs
}

func pgUsageDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.UsageLog{}); err != nil {
		t.Fatal(err)
	}
	return db
}

// Completed-only SECRET on the REAL native 3A strict stream: nothing is
// released before the final check, so the withheld buffer plus zero content
// leakage is assertable end-to-end.
func TestCodexRetestPlaygroundResponsesNativeStrictBlocksCompletedOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, gs := pgRespGuardSetup(t, "strict")
	p := &pgRespNativeProvider{name: "native", sse: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"SECRET\"}]}]}}\n\n"}
	h := pgNativeHandler(t, db, gs, p)
	w := pgNativeRequest(t, h, true, nil)
	body := w.Body.String()
	if strings.Contains(body, "SECRET") {
		t.Fatalf("native strict stream leaked completed-only content: %s", body)
	}
	if !strings.Contains(body, "guardrail_blocked") {
		t.Fatalf("native strict stream did not block: %s", body)
	}
	if p.calls != 1 {
		t.Fatalf("expected one upstream call, got %d", p.calls)
	}
}

// Detector failure (fail-closed) on the native 3A strict stream: the buffered
// benign content must NOT be released — only the guardrail error event.
func TestCodexRetestPlaygroundResponsesNativeFailClosedReleasesNothing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gs := pgFailingGuardrailSvc(t, false)
	p := &pgRespNativeProvider{name: "native", sse: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"benign hello\"}\n\n"}
	h := pgNativeHandler(t, pgUsageDB(t), gs, p)
	w := pgNativeRequest(t, h, true, nil)
	body := w.Body.String()
	if strings.Contains(body, "benign hello") {
		t.Fatalf("fail-closed detector released unverified content: %s", body)
	}
	if !strings.Contains(body, "guardrail_error") {
		t.Fatalf("fail-closed detector did not surface guardrail error: %s", body)
	}
}

// Request-context cancellation mid-stream takes the same fail-closed branch:
// the strict buffer is withheld entirely.
func TestCodexRetestPlaygroundResponsesNativeCtxCancelFailClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, gs := pgRespGuardSetup(t, "strict")
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &pgRespNativeProvider{name: "native", cancelFn: cancel,
		sse: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"benign hello\"}\n\n"}
	h := pgNativeHandler(t, db, gs, p)
	w := pgNativeRequest(t, h, true, reqCtx)
	body := w.Body.String()
	if strings.Contains(body, "benign hello") {
		t.Fatalf("cancelled check released unverified content: %s", body)
	}
	if !strings.Contains(body, "guardrail_error") {
		t.Fatalf("cancelled check did not fail closed: %s", body)
	}
}

// Non-stream 3A raw body with escaped tool arguments: decoded detection on the
// real native path.
func TestCodexRetestPlaygroundResponsesNativeNonStreamBlocksDecodedArgs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, gs := pgRespGuardSetup(t, "window")
	p := &pgRespNativeProvider{name: "native", body: `{"output":[{"type":"function_call","arguments":"{\"value\":\"SE\u0043RET\"}"}]}`}
	h := pgNativeHandler(t, db, gs, p)
	w := pgNativeRequest(t, h, false, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "guardrail_blocked") {
		t.Fatalf("native non-stream decoded args bypassed block: %d %s", w.Code, w.Body.String())
	}
}

// Non-stream detector failure (fail-closed): 503, body withheld. The 3B
// translation path uses the same helper.
func TestCodexRetestPlaygroundResponsesNonStreamFailClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gs := pgFailingGuardrailSvc(t, false)
	reg := provider.NewRegistry()
	chat := &pgRespGuardProvider{name: "fake"} // benign non-tool response
	reg.Register("fake", chat)
	resolver := router.NewResolver(reg, &pgModelRepo{}, nil, map[router.StrategyName]router.RoutingStrategy{
		router.StrategyWeightedRandom: &router.WeightedRandomStrategy{},
	}, nil, nil, nil, nil)
	h := NewPlaygroundHandler(resolver, service.NewUsageService(repository.NewUsageLogRepo(pgUsageDB(t))), nil, nil, nil, gs, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/api/playground/responses", strings.NewReader(`{"model":"test-model","input":"hi"}`))
	c.Set("org_id", int64(7))
	h.Responses(c)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "guardrail_error") {
		t.Fatalf("non-stream detector failure did not fail closed: %d %s", w.Code, w.Body.String())
	}
	if chat.calls != 1 {
		t.Fatalf("expected one upstream call before the check, got %d", chat.calls)
	}
}

// 3B translated strict stream with a failing detector: the buffered events
// are withheld — only the guardrail error reaches the client.
func TestCodexRetestPlaygroundResponses3BStreamFailClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gs := pgFailingGuardrailSvc(t, false) // strict + fail-closed
	db := pgUsageDB(t)
	reg := provider.NewRegistry()
	p := &pgRespGuardProvider{name: "fake"}
	reg.Register("fake", p)
	resolver := router.NewResolver(reg, &pgModelRepo{}, nil, map[router.StrategyName]router.RoutingStrategy{
		router.StrategyWeightedRandom: &router.WeightedRandomStrategy{},
	}, nil, nil, nil, nil)
	h := NewPlaygroundHandler(resolver, service.NewUsageService(repository.NewUsageLogRepo(db)), nil, nil, nil, gs, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/api/playground/responses", strings.NewReader(`{"model":"test-model","input":"hi","stream":true}`))
	c.Set("org_id", int64(7))
	h.Responses(c)
	body := w.Body.String()
	if strings.Contains(body, `"delta"`) {
		t.Fatalf("3B fail-closed stream released buffered events: %s", body)
	}
	if !strings.Contains(body, "guardrail_error") {
		t.Fatalf("3B fail-closed stream did not surface guardrail error: %s", body)
	}
}

// Explicit fail-open with a failing detector: content is released per policy.
func TestCodexRetestPlaygroundResponsesFailOpenReleasesPerPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gs := pgFailingGuardrailSvc(t, true)
	p := &pgRespNativeProvider{name: "native", sse: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"benign hello\"}\n\n"}
	h := pgNativeHandler(t, pgUsageDB(t), gs, p)
	w := pgNativeRequest(t, h, true, nil)
	if !strings.Contains(w.Body.String(), "benign hello") {
		t.Fatalf("explicit fail-open did not release per policy: %s", w.Body.String())
	}
}
