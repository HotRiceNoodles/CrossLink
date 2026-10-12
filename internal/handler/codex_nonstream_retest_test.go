package handler

// Retest regressions for the round-10 review findings: non-stream output and
// idempotent-replay paths must detect tool-argument JSON escapes in DECODED
// form (the tool itself decodes {"value":"SE\u0043RET"} to "SECRET"), must
// mask with a safe canonical write-back, must surface partial coverage
// (truncated) flags, and an idempotent replay rejected by current policy must
// never re-execute the upstream operation.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// fakeNonStreamProvider returns one canned non-stream response and counts
// upstream executions.
type fakeNonStreamProvider struct {
	name     string
	resp     *domain.OpenAIResponse
	anthResp any
	calls    int
}

func (f *fakeNonStreamProvider) Chat(context.Context, *domain.OpenAIRequest, string) (*domain.OpenAIResponse, error) {
	f.calls++
	cp := *f.resp
	return &cp, nil
}

func (f *fakeNonStreamProvider) StreamChat(context.Context, *domain.OpenAIRequest, string) (<-chan domain.SSEChunk, error) {
	return nil, nil
}
func (f *fakeNonStreamProvider) Name() string { return f.name }

func retestSetup(t *testing.T, ruleType, action, config string) (*gorm.DB, *guardrail.GuardrailService) {
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
		"INSERT INTO system_settings VALUES ('guardrails_enabled','true')",
		"CREATE TABLE guardrail_rules (id INTEGER PRIMARY KEY, name TEXT, type TEXT, direction TEXT, enabled BOOLEAN, config TEXT, severity TEXT, action TEXT)",
		"INSERT INTO guardrail_rules VALUES (1,'synthetic',?, 'response',true,?,'high',?)",
	} {
		if err := db.Exec(stmt, ruleType, config, action).Error; err != nil {
			t.Fatal(err)
		}
	}
	gs := guardrail.NewGuardrailService(db, nil)
	gs.RefreshSettings(t.Context())
	return db, gs
}

func retestRoutes(p provider.Provider) []*router.RouteResult {
	return []*router.RouteResult{{Provider: p, ProviderModel: "synthetic", ProviderRow: &model.Provider{}}}
}

// escapedToolArgs is a JSON arguments string whose \u0043 escape decodes to
// "SECRET" / restores the credential prefix when parsed by the tool.
const escapedToolArgs = `{"value":"SE\u0043RET"}`

func TestCodexRetestNonStreamBlocksDecodedToolArguments(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, gs := retestSetup(t, "keyword_filter", "block", `{"blocklist":["SECRET"]}`)
	p := &fakeNonStreamProvider{name: "retest", resp: &domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{{
			Message: domain.OpenAIMessage{
				Role: "assistant",
				ToolCalls: []domain.OpenAIToolCall{{
					ID: "c1", Type: "function",
					Function: domain.OpenAIFunctionCall{Name: "send", Arguments: escapedToolArgs},
				}},
			},
		}},
	}}
	h := &OpenAIHandler{guardrailSvc: gs, usageSvc: service.NewUsageService(repository.NewUsageLogRepo(db))}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	h.handleNonStream(c, retestRoutes(p), &domain.OpenAIRequest{Model: "synthetic", Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}, nil, 0, time.Now(), "retest")
	if p.calls != 1 {
		t.Fatalf("expected one upstream generation, got %d", p.calls)
	}
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "guardrail_blocked") {
		t.Fatalf("decoded tool argument bypassed non-stream block: %d %s", w.Code, w.Body.String())
	}
}

func TestCodexRetestNonStreamMasksDecodedToolArguments(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, gs := retestSetup(t, "credential_detection", "mask", `{}`)
	escapedCred := `{"value":"sk_liv\u0065_` + strings.Repeat("a", 30) + `"}` // decodes to sk_live_aaa…
	p := &fakeNonStreamProvider{name: "retest", resp: &domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{{
			Message: domain.OpenAIMessage{
				Role: "assistant",
				ToolCalls: []domain.OpenAIToolCall{{
					ID: "c1", Type: "function",
					Function: domain.OpenAIFunctionCall{Name: "send", Arguments: escapedCred},
				}},
			},
		}},
	}}
	h := &OpenAIHandler{guardrailSvc: gs, usageSvc: service.NewUsageService(repository.NewUsageLogRepo(db))}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	h.handleNonStream(c, retestRoutes(p), &domain.OpenAIRequest{Model: "synthetic", Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}, nil, 0, time.Now(), "retest")
	if w.Code != http.StatusOK {
		t.Fatalf("mask path failed: %d %s", w.Code, w.Body.String())
	}
	var resp domain.OpenAIResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("masked response is not valid JSON: %v", err)
	}
	args := resp.Choices[0].Message.ToolCalls[0].Function.Arguments
	if !json.Valid([]byte(args)) {
		t.Fatalf("masked tool arguments are not valid JSON: %s", args)
	}
	if strings.Contains(w.Body.String(), "sk_live_") || strings.Contains(args, "sk_liv") {
		t.Fatalf("original decoded credential survived masking: %s", w.Body.String())
	}
	if v, ok := c.Get("guardrail_triggered"); !ok || v != true {
		t.Fatal("mask trigger flag missing")
	}
}

func TestCodexRetestIdemReplayRejectsDecodedToolArgumentsWithoutReexec(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, gs := retestSetup(t, "keyword_filter", "block", `{"blocklist":["SECRET"]}`)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	idem := service.NewIdempotencyCache(rdb)

	cachedBody, _ := json.Marshal(&domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{{
			Message: domain.OpenAIMessage{
				Role: "assistant",
				ToolCalls: []domain.OpenAIToolCall{{
					ID: "c1", Type: "function",
					Function: domain.OpenAIFunctionCall{Name: "send", Arguments: escapedToolArgs},
				}},
			},
		}},
	})
	if err := idem.Set(t.Context(), 42, "retest-key", &service.CachedResponse{StatusCode: http.StatusOK, Body: cachedBody}); err != nil {
		t.Fatal(err)
	}

	p := &fakeNonStreamProvider{name: "retest", resp: &domain.OpenAIResponse{Choices: []domain.OpenAIChoice{{Message: domain.OpenAIMessage{Role: "assistant", Content: "regenerated"}}}}}
	h := &OpenAIHandler{guardrailSvc: gs, usageSvc: service.NewUsageService(repository.NewUsageLogRepo(db)), idemCache: idem}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("api_key", &model.APIKey{ID: 42})
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request.Header.Set("X-Idempotency-Key", "retest-key")
	h.handleNonStream(c, retestRoutes(p), &domain.OpenAIRequest{Model: "synthetic", Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}, nil, 0, time.Now(), "retest")

	if p.calls != 0 {
		t.Fatalf("idempotent replay re-executed upstream %d times under changed policy", p.calls)
	}
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "guardrail_blocked") {
		t.Fatalf("policy-rejected replay must reply 400, got %d %s", w.Code, w.Body.String())
	}
}

func TestCodexRetestNonStreamSurfacesTruncatedFlag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, gs := retestSetup(t, "keyword_filter", "log", `{"blocklist":["SECRET"]}`)
	huge := strings.Repeat("x", 500001+10) // > maxCheckContent (500K runes)
	p := &fakeNonStreamProvider{name: "retest", resp: &domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{{Message: domain.OpenAIMessage{Role: "assistant", Content: huge}}},
	}}
	h := &OpenAIHandler{guardrailSvc: gs, usageSvc: service.NewUsageService(repository.NewUsageLogRepo(db))}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	h.handleNonStream(c, retestRoutes(p), &domain.OpenAIRequest{Model: "synthetic", Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}, nil, 0, time.Now(), "retest")
	if v, ok := c.Get("guardrail_truncated"); !ok || v != true {
		t.Fatalf("non-stream partial coverage not surfaced (flag=%v ok=%v)", v, ok)
	}
	_ = w
}

// --- Messages (Anthropic protocol) generation + idempotent replay real-path
// regressions (round-11 finding 4). ---

// retestModelRepo serves "test-model" from a fake provider row.
type retestModelRepo struct{}

func (m *retestModelRepo) FindByModelName(_ context.Context, name string, _ int64) ([]model.ProviderModel, error) {
	if name != "test-model" {
		return nil, nil
	}
	return []model.ProviderModel{{
		ID: 1, ModelName: name, ProviderModel: "fake-model", Weight: 100, Priority: 1, Status: 1,
		Provider: model.Provider{Name: "fake", Status: 1},
	}}, nil
}

func retestMessagesHandler(t *testing.T, db *gorm.DB, gs *guardrail.GuardrailService, idem *service.IdempotencyCache, p provider.Provider) (*AnthropicHandler, *fakeNonStreamProvider) {
	t.Helper()
	fake, _ := p.(*fakeNonStreamProvider)
	reg := provider.NewRegistry()
	reg.Register("fake", p)
	resolver := router.NewResolver(reg, &retestModelRepo{}, nil, map[router.StrategyName]router.RoutingStrategy{
		router.StrategyWeightedRandom: &router.WeightedRandomStrategy{},
	}, nil, nil, nil, nil)
	return &AnthropicHandler{
		svc:          service.NewGatewayService(resolver, nil, nil, nil, nil),
		resolver:     resolver,
		usageSvc:     service.NewUsageService(repository.NewUsageLogRepo(db)),
		idemCache:    idem,
		guardrailSvc: gs,
	}, fake
}

func retestMessagesPost(t *testing.T, h *AnthropicHandler, idemKey string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("api_key", &model.APIKey{ID: 42})
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"test-model","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
	if idemKey != "" {
		c.Request.Header.Set("X-Idempotency-Key", idemKey)
	}
	h.HandleMessages(c)
	return w
}

func TestCodexRetestMessagesNonStreamBlocksDecodedToolUseInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, gs := retestSetup(t, "keyword_filter", "block", `{"blocklist":["SECRET"]}`)
	p := &fakeNonStreamProvider{name: "retest", resp: &domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{{
			Message: domain.OpenAIMessage{
				Role: "assistant",
				ToolCalls: []domain.OpenAIToolCall{{
					ID: "c1", Type: "function",
					Function: domain.OpenAIFunctionCall{Name: "send", Arguments: escapedToolArgs},
				}},
			},
		}},
	}}
	h, fake := retestMessagesHandler(t, db, gs, nil, p)
	w := retestMessagesPost(t, h, "")
	if fake.calls != 1 {
		t.Fatalf("expected one upstream generation, got %d", fake.calls)
	}
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "guardrail_blocked") {
		t.Fatalf("decoded tool_use input bypassed messages block: %d %s", w.Code, w.Body.String())
	}
}

func TestCodexRetestMessagesNonStreamMasksDecodedToolUseInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, gs := retestSetup(t, "credential_detection", "mask", `{}`)
	escapedCred := `{"value":"sk_liv\u0065_` + strings.Repeat("a", 30) + `"}` // decodes to sk_live_aaa…
	p := &fakeNonStreamProvider{name: "retest", resp: &domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{{
			Message: domain.OpenAIMessage{
				Role: "assistant",
				ToolCalls: []domain.OpenAIToolCall{{
					ID: "c1", Type: "function",
					Function: domain.OpenAIFunctionCall{Name: "send", Arguments: escapedCred},
				}},
			},
		}},
	}}
	h, _ := retestMessagesHandler(t, db, gs, nil, p)
	w := retestMessagesPost(t, h, "")
	if w.Code != http.StatusOK {
		t.Fatalf("messages mask path failed: %d %s", w.Code, w.Body.String())
	}
	var resp domain.AnthropicResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("masked messages response is not valid JSON: %v", err)
	}
	for _, block := range resp.Content {
		if block.Type == "tool_use" {
			if !json.Valid(block.Input) {
				t.Fatalf("masked tool_use input is not valid JSON: %s", block.Input)
			}
			if strings.Contains(string(block.Input), "sk_liv") {
				t.Fatalf("decoded credential survived masking: %s", block.Input)
			}
		}
	}
	if strings.Contains(w.Body.String(), "sk_live_") {
		t.Fatalf("masked messages body leaked credential: %s", w.Body.String())
	}
}

func TestCodexRetestMessagesIdemReplayRejectsWithoutReexec(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, gs := retestSetup(t, "keyword_filter", "block", `{"blocklist":["SECRET"]}`)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	idem := service.NewIdempotencyCache(rdb)

	cachedBody := []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"send","input":{"value":"SE\u0043RET"}}]}`)
	if err := idem.Set(t.Context(), 42, "msg-key", &service.CachedResponse{StatusCode: http.StatusOK, Body: cachedBody}); err != nil {
		t.Fatal(err)
	}
	p := &fakeNonStreamProvider{name: "retest", resp: &domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{{Message: domain.OpenAIMessage{Role: "assistant", Content: "regenerated"}}},
	}}
	h, fake := retestMessagesHandler(t, db, gs, idem, p)
	w := retestMessagesPost(t, h, "msg-key")
	if fake.calls != 0 {
		t.Fatalf("messages idempotent replay re-executed upstream %d times under changed policy", fake.calls)
	}
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "guardrail_blocked") {
		t.Fatalf("policy-rejected messages replay must reply 400, got %d %s", w.Code, w.Body.String())
	}
}
