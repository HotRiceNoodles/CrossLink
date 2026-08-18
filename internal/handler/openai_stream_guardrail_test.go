package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// fakeStreamProvider emits a fixed, pre-buffered sequence of SSE chunks. It
// satisfies provider.Provider without any network.
type fakeStreamProvider struct {
	name   string
	chunks []domain.SSEChunk
}

func (f *fakeStreamProvider) Chat(context.Context, *domain.OpenAIRequest, string) (*domain.OpenAIResponse, error) {
	return nil, nil
}
func (f *fakeStreamProvider) StreamChat(context.Context, *domain.OpenAIRequest, string) (<-chan domain.SSEChunk, error) {
	ch := make(chan domain.SSEChunk, len(f.chunks))
	for _, c := range f.chunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}
func (f *fakeStreamProvider) Name() string { return f.name }

var _ provider.Provider = (*fakeStreamProvider)(nil)

// TestHandleStream_GuardrailBlock_PublishesTokensForTPMReconcile is a regression
// test for the C6-closure defect (P1): the guardrail pure-block early-return
// path in handleStream used to bypass c.Set("input_tokens"/"output_tokens"),
// leaving the TPM reservation made by TPMLimit permanently consumed (ReportTokens
// saw total=0 and bailed) — sustained guardrail hits drained TPM quota (DoS).
//
// The fix is a defer that publishes token usage on every exit path. This test
// drives the exact defect path:
//   - guardrail enabled + fail-closed, with a real-but-empty sqlite DB so
//     LoadRules errors cleanly (no table);
//   - the stream wrapper calls Check on the terminal Done chunk, Check errors,
//     and (failOpen=false) the wrapper returns Blocked (action="");
//   - handleStream takes the pure-block branch and returns early.
//
// It then asserts the defer populated input_tokens/output_tokens so ReportTokens
// can reconcile. On the pre-fix code this assertion fails (tokens never set).
func TestHandleStream_GuardrailBlock_PublishesTokensForTPMReconcile(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Real sqlite, but do NOT migrate — so Find on guardrail_rules / system_settings
	// errors (table missing) instead of panicking on a nil *gorm.DB.
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	gs := guardrail.NewGuardrailService(db, nil)
	gs.SetEnabled(true)
	gs.SetFailOpen(false) // Check error ⇒ wrapper returns Blocked

	h := &OpenAIHandler{
		guardrailSvc: gs,
		usageSvc:     service.NewUsageService(nil),
		// resolver/health/classifier/budget intentionally nil:
		//   - ExpandFallbackRoutes returns early when resolver == nil
		//   - NewFallbackEngine/SetClassifier tolerate nil
		//   - WithRetry never touches budget when NumRetries == 0
	}

	p := &fakeStreamProvider{name: "fake", chunks: []domain.SSEChunk{
		{Chunk: &domain.OpenAIChunk{Choices: []domain.OpenAIChunkChoice{{
			Delta: domain.OpenAIChunkDelta{Content: "hello world response"},
		}}}},
		{Done: true},
	}}
	routes := []*router.RouteResult{{
		Provider:      p,
		ProviderModel: "fake-model",
		ProviderRow:   &model.Provider{},
	}}

	req := &domain.OpenAIRequest{
		Stream:  true,
		Model:   "fake-model",
		Messages: []domain.OpenAIMessage{{Role: "user", Content: "please respond to this request"}},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	h.handleStream(c, routes, req, nil, 0, time.Now(), "test-session")

	it, ok := c.Get("input_tokens")
	if !ok {
		t.Fatal("input_tokens not published on guardrail-block early return; TPM reservation would leak")
	}
	if v, _ := it.(int); v <= 0 {
		t.Fatalf("input_tokens = %v, want > 0 (defer's estimation fallback should run)", it)
	}
	ot, ok := c.Get("output_tokens")
	if !ok {
		t.Fatal("output_tokens not published on guardrail-block early return")
	}
	if v, _ := ot.(int); v < 0 {
		t.Fatalf("output_tokens = %v, want >= 0", ot)
	}
	if _, ok := c.Get("guardrail_triggered"); !ok {
		t.Error("expected guardrail_triggered to be set (confirms the pure-block path was taken)")
	}
}
