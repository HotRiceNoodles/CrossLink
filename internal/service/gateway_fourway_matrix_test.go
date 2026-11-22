package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/translator"
)

// M3b four-direction acceptance matrix: every inbound protocol × every
// upstream protocol, over REAL adapters (httptest upstreams), the REAL
// bidirectional translators, and the REAL GatewayService attempt loop —
// verifying the M0 sampling rules and the M3 envelope carry end-to-end
// instead of at isolated units.

// capturedRequest records what an httptest upstream actually received.
type capturedRequest struct {
	mu    sync.Mutex
	bodie []map[string]any
	hits  int
}

func (cr *capturedRequest) record(body []byte) {
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	cr.mu.Lock()
	cr.bodie = append(cr.bodie, m)
	cr.hits++
	cr.mu.Unlock()
}

func (cr *capturedRequest) last() map[string]any {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if len(cr.bodie) == 0 {
		return nil
	}
	return cr.bodie[len(cr.bodie)-1]
}

func (cr *capturedRequest) count() int {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return cr.hits
}

// openAIUpstream answers the OpenAI chat protocol and captures request bodies.
func openAIUpstream(t *testing.T, status int) (*httptest.Server, *capturedRequest) {
	t.Helper()
	cr := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		cr.record(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, cr
}

// anthropicUpstream answers the Anthropic messages protocol and captures
// request bodies + the anthropic-version header.
func anthropicUpstream(t *testing.T, status int) (*httptest.Server, *capturedRequest, *string) {
	t.Helper()
	cr := &capturedRequest{}
	var versionHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		cr.record(body)
		versionHeader = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, cr, &versionHeader
}

func matrixResolver(reg *provider.Registry, mappings ...model.ProviderModel) *router.Resolver {
	repo := &mockRepo{data: map[string][]model.ProviderModel{}}
	for _, m := range mappings {
		repo.data[m.ModelName] = append(repo.data[m.ModelName], m)
	}
	return router.NewResolver(reg, repo, nil, map[router.StrategyName]router.RoutingStrategy{
		router.StrategyWeightedRandom: &router.WeightedRandomStrategy{},
	}, nil, nil, nil, nil)
}

func f64(v float64) *float64 { return &v }
func i64p(v int) *int        { return &v }

// ---- Direction matrix: Anthropic inbound via the real GatewayService ----

func TestFourWay_AnthropicInbound_ToOpenAIUpstream(t *testing.T) {
	t.Parallel()
	srv, cr := openAIUpstream(t, http.StatusOK)
	reg := provider.NewRegistry()
	reg.Register("oa", provider.NewOpenAICompatible("oa", srv.URL, 5*time.Second))
	resolver := matrixResolver(reg, model.ProviderModel{
		ModelName: "m1", ProviderModel: "oa-model", Weight: 1, Status: 1,
		Provider: model.Provider{Name: "oa", Status: 1, ID: 1},
	})
	svc := NewGatewayService(resolver, reg, nil, nil, nil)

	// Non-stream: sampling params translate to the OpenAI wire form.
	req := &domain.AnthropicRequest{
		Model: "m1", MaxTokens: 32, Temperature: f64(0.5),
		Messages: makeMessages("hi"),
	}
	if _, err := svc.Chat(context.Background(), req, "", 0); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	last := cr.last()
	if last["temperature"] != 0.5 {
		t.Fatalf("upstream temperature = %v, want 0.5", last["temperature"])
	}

	// Stream: same direction through the streaming pipeline.
	reqS := &domain.AnthropicRequest{
		Model: "m1", MaxTokens: 32, Stream: true, Temperature: f64(0.7),
		Messages: makeMessages("hi"),
	}
	_, serr := svc.StreamChat(context.Background(), reqS, func(_ context.Context, _ StreamEvent) bool { return true }, "", 0)
	if serr != nil {
		t.Fatalf("StreamChat: %v", serr)
	}
	if got := cr.last()["stream"]; got != true {
		t.Fatalf("upstream stream flag = %v, want true", got)
	}
}

func TestFourWay_AnthropicInbound_ToAnthropicUpstream_OldVersion(t *testing.T) {
	t.Parallel()
	srv, cr, ver := anthropicUpstream(t, http.StatusOK)
	reg := provider.NewRegistry()
	anth, err := provider.NewAnthropicProvider("an", srv.URL, "k", []byte(`{"api_version":"2023-06-01"}`), 5*time.Second)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	reg.Register("an", anth)
	resolver := matrixResolver(reg, model.ProviderModel{
		ModelName: "m2", ProviderModel: "an-model", Weight: 1, Status: 1,
		Provider: model.Provider{Name: "an", Status: 1, ID: 2},
	})
	svc := NewGatewayService(resolver, reg, nil, nil, nil)

	// Old version: sampling pass through unchanged (temperature arrives).
	req := &domain.AnthropicRequest{
		Model: "m2", MaxTokens: 32, Temperature: f64(0.5), TopK: i64p(40),
		Messages: makeMessages("hi"),
	}
	if _, err := svc.Chat(context.Background(), req, "", 0); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := cr.last()["temperature"]; got != 0.5 {
		t.Fatalf("old-version upstream temperature = %v, want 0.5", got)
	}
	if *ver != "2023-06-01" {
		t.Fatalf("anthropic-version header = %q", *ver)
	}
	// Documented pre-existing behavior: top_k drops at translation even for
	// old-version upstreams (M0 carry exists for the version check only).
	if _, has := cr.last()["top_k"]; has {
		t.Fatal("top_k unexpectedly present on the wire (documented drop)")
	}
}

func TestFourWay_AnthropicInbound_ToAnthropicUpstream_NewVersion_SamplingTiers(t *testing.T) {
	t.Parallel()
	srv, cr, ver := anthropicUpstream(t, http.StatusOK)
	reg := provider.NewRegistry()
	anth, err := provider.NewAnthropicProvider("an26", srv.URL, "k", []byte(`{"api_version":"2026-10-21"}`), 5*time.Second)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	reg.Register("an26", anth)
	resolver := matrixResolver(reg, model.ProviderModel{
		ModelName: "m3", ProviderModel: "an-model", Weight: 1, Status: 1,
		Provider: model.Provider{Name: "an26", Status: 1, ID: 3},
	})
	svc := NewGatewayService(resolver, reg, nil, nil, nil)

	newReq := func(temp *float64, topK *int) *domain.AnthropicRequest {
		return &domain.AnthropicRequest{
			Model: "m3", MaxTokens: 32, Temperature: temp, TopK: topK,
			Messages: makeMessages("hi"),
		}
	}

	// Tier 1 — default temperature (1.0): silently stripped, request succeeds.
	if _, err := svc.Chat(context.Background(), newReq(f64(1.0), nil), "", 0); err != nil {
		t.Fatalf("default temperature must pass: %v", err)
	}
	if _, has := cr.last()["temperature"]; has {
		t.Fatal("default temperature must be stripped on the wire")
	}

	// Tier 2 — non-default temperature: explicit rejection through the full
	// chain (adapter checkpoint → engine exhaustion → service error).
	_, err = svc.Chat(context.Background(), newReq(f64(0.5), nil), "", 0)
	if err == nil || !strings.Contains(err.Error(), "sampling parameter") {
		t.Fatalf("non-default temperature must surface the sampling rejection, got %v", err)
	}

	// Tier 3 — top_k via the M0 carry: rejected although the reverse
	// translation cannot express it on the wire.
	_, err = svc.Chat(context.Background(), newReq(nil, i64p(40)), "", 0)
	if err == nil || !strings.Contains(err.Error(), "sampling parameter") {
		t.Fatalf("top_k must be rejected via the carry, got %v", err)
	}
	if *ver != "2026-10-21" {
		t.Fatalf("anthropic-version header = %q", *ver)
	}
}

func TestFourWay_AnthropicInbound_ToAnthropicUpstream_CompatStrip(t *testing.T) {
	t.Parallel()
	srv, cr, _ := anthropicUpstream(t, http.StatusOK)
	reg := provider.NewRegistry()
	anth, err := provider.NewAnthropicProvider("anStrip", srv.URL, "k", []byte(`{"api_version":"2026-10-21","sampling_compat_strip":true}`), 5*time.Second)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	reg.Register("anStrip", anth)
	resolver := matrixResolver(reg, model.ProviderModel{
		ModelName: "m4", ProviderModel: "an-model", Weight: 1, Status: 1,
		Provider: model.Provider{Name: "anStrip", Status: 1, ID: 4},
	})
	svc := NewGatewayService(resolver, reg, nil, nil, nil)

	req := &domain.AnthropicRequest{
		Model: "m4", MaxTokens: 32, Temperature: f64(0.5), TopK: i64p(40),
		Messages: makeMessages("hi"),
	}
	if _, err := svc.Chat(context.Background(), req, "", 0); err != nil {
		t.Fatalf("compat strip must not reject: %v", err)
	}
	last := cr.last()
	if _, has := last["temperature"]; has {
		t.Fatal("compat strip must remove temperature on the wire")
	}
	if _, has := last["top_k"]; has {
		t.Fatal("compat strip must remove top_k on the wire")
	}
}

// ---- Direction matrix: Chat inbound against a real Anthropic adapter ----

func TestFourWay_ChatInbound_ToAnthropicUpstream(t *testing.T) {
	t.Parallel()
	srv, cr, ver := anthropicUpstream(t, http.StatusOK)
	oldP, err := provider.NewAnthropicProvider("anOld", srv.URL, "k", []byte(`{"api_version":"2023-06-01"}`), 5*time.Second)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if _, err := oldP.Chat(context.Background(), &domain.OpenAIRequest{
		Model: "m", MaxTokens: i64p(32), Temperature: f64(0.5),
		Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}},
	}, "k"); err != nil {
		t.Fatalf("old-version chat inbound: %v", err)
	}
	if got := cr.last()["temperature"]; got != 0.5 {
		t.Fatalf("old-version upstream temperature = %v, want 0.5", got)
	}
	if *ver != "2023-06-01" {
		t.Fatalf("old-version anthropic-version header = %q", *ver)
	}

	// New version: Chat-inbound temperature must hit the same checkpoint —
	// and the rejection must happen BEFORE any upstream call (the captured
	// header therefore still shows the old-version request above).
	newP, err := provider.NewAnthropicProvider("anNew", srv.URL, "k", []byte(`{"api_version":"2026-10-21"}`), 5*time.Second)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	before := cr.count()
	_, err = newP.Chat(context.Background(), &domain.OpenAIRequest{
		Model: "m", MaxTokens: i64p(32), Temperature: f64(0.5),
		Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}},
	}, "k")
	if err == nil || !strings.Contains(err.Error(), "sampling parameter") {
		t.Fatalf("chat-inbound non-default temperature must be rejected, got %v", err)
	}
	if cr.count() != before {
		t.Fatal("rejection must happen BEFORE the upstream call")
	}
}

// ---- Direction matrix: OpenAI→OpenAI baseline ----

func TestFourWay_OpenAIInbound_ToOpenAIUpstream(t *testing.T) {
	t.Parallel()
	srv, cr := openAIUpstream(t, http.StatusOK)
	p := provider.NewOpenAICompatible("oo", srv.URL, 5*time.Second)
	if _, err := p.Chat(context.Background(), &domain.OpenAIRequest{
		Model: "m", MaxTokens: i64p(32), Temperature: f64(0.9),
		Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}},
	}, "k"); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if got := cr.last()["temperature"]; got != 0.9 {
		t.Fatalf("upstream temperature = %v, want 0.9", got)
	}
}

// ---- Fallback dimension: cheap OpenAI primary fails → Anthropic fallback ----

func TestFourWay_Fallback_OpenAIPrimary_ToAnthropicFallback(t *testing.T) {
	t.Parallel()
	badSrv, primaryHits := openAIUpstream(t, http.StatusInternalServerError)
	anSrv, cr, _ := anthropicUpstream(t, http.StatusOK)

	reg := provider.NewRegistry()
	reg.Register("bad", provider.NewOpenAICompatible("bad", badSrv.URL, 5*time.Second))
	anth, err := provider.NewAnthropicProvider("fb", anSrv.URL, "k", []byte(`{"api_version":"2023-06-01"}`), 5*time.Second)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	reg.Register("fb", anth)
	resolver := matrixResolver(reg,
		// NOTE: distinct mapping IDs are REQUIRED — the weighted strategy
		// excludes remaining candidates by ModelID, and zero-value IDs collapse
		// the secondary route out of the chain.
		model.ProviderModel{ID: 51, ModelName: "m5", ProviderModel: "p1", Weight: 5, Priority: 1, Status: 1, Provider: model.Provider{Name: "bad", Status: 1, ID: 5}},
		model.ProviderModel{ID: 52, ModelName: "m5", ProviderModel: "p2", Weight: 1, Priority: 2, Status: 1, Provider: model.Provider{Name: "fb", Status: 1, ID: 6}},
	)
	svc := NewGatewayService(resolver, reg, nil, nil, nil)

	res, err := svc.Chat(context.Background(), &domain.AnthropicRequest{
		Model: "m5", MaxTokens: 32, Messages: makeMessages("hi"),
	}, "", 0)
	if err != nil {
		t.Fatalf("fallback chain must succeed: %v", err)
	}
	if res.ProviderName != "fb" {
		t.Fatalf("result provider = %q, want fallback fb", res.ProviderName)
	}
	// Weighted-random primary pick: ~1/6 runs pick fb as the PRIMARY (it
	// then succeeds immediately and the failing primary is never consulted)
	// — that is the strategy working as designed, not a broken chain. Only
	// the bad-first interleaving requires both upstreams to have been tried.
	if primaryHits.count() == 0 && cr.count() > 0 {
		t.Log("fb won the weighted primary pick; failing primary skipped (valid)")
	} else if primaryHits.count() == 0 || cr.count() == 0 {
		t.Fatal("both primary and fallback upstreams must have been tried")
	}
}

// ---- P3 end-to-end: registered dialect param rides the full chain ----

func TestFourWay_DialectParamEndToEnd(t *testing.T) {
	t.Parallel()
	srv, cr := openAIUpstream(t, http.StatusOK)
	reg := provider.NewRegistry()
	reg.Register("oa", provider.NewOpenAICompatible("oa", srv.URL, 5*time.Second))

	// Route opts in to forwarding enable_thinking.
	repo := &mockRepo{data: map[string][]model.ProviderModel{
		"qwen3": {{
			ID: 61, ModelName: "qwen3", ProviderModel: "qwen3-max", Weight: 1, Status: 1,
			Provider: model.Provider{Name: "oa", Status: 1, ID: 61},
		}},
	}}
	resolver := router.NewResolver(reg, repo, nil, map[router.StrategyName]router.RoutingStrategy{
		router.StrategyWeightedRandom: &router.WeightedRandomStrategy{},
	}, nil, nil, nil, nil)
	svc := NewGatewayService(resolver, reg, nil, nil, nil)

	// Client sends enable_thinking; admission carries it on the envelope
	// (source=openai wire form is what anthropic→openai translation would
	// produce too — here we set it the way the openai handler would).
	req := &domain.AnthropicRequest{
		Model: "qwen3", MaxTokens: 32, Messages: makeMessages("hi"),
		Extensions: &domain.ProtocolExtensions{
			SourceProtocol: "openai",
			Params:         map[string]json.RawMessage{"dialect.enable_thinking": json.RawMessage(`true`)},
		},
	}

	// WITHOUT the route opt-in the param must not reach the wire.
	if _, err := svc.Chat(context.Background(), req, "", 0); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if _, has := cr.last()["enable_thinking"]; has {
		t.Fatal("dialect param must NOT reach the upstream without allowed_extensions opt-in")
	}

	// WITH the opt-in (via request_overrides, which implies it) it lands on
	// the wire with the admin's value winning over the client's.
	// datatypes.JSON unmarshals as raw bytes (json.RawMessage semantics) —
	// set the field without importing the gorm datatypes package.
	_ = json.Unmarshal([]byte(`{"request_overrides":{"enable_thinking":false}}`), &repo.data["qwen3"][0].ExtraConfig)
	resolver.Invalidate()
	if _, err := svc.Chat(context.Background(), req, "", 0); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := cr.last()["enable_thinking"]; got != false {
		t.Fatalf("upstream enable_thinking = %v, want false (admin override wins)", got)
	}
}

// ---- Envelope carry across the real translator ----

func TestFourWay_EnvelopeSurvivesTranslation(t *testing.T) {
	t.Parallel()
	env := &domain.ProtocolExtensions{
		SourceProtocol: "anthropic",
		Headers:        map[string]string{"anthropic-beta": "demo"},
	}
	req := &domain.AnthropicRequest{
		Model: "m", MaxTokens: 8, Messages: makeMessages("hi"), Extensions: env,
	}
	oai, err := translator.AnthropicToOpenAI(req, "m")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if oai.Extensions != env {
		t.Fatal("envelope must survive AnthropicToOpenAI verbatim (design §1.1)")
	}
	// And it must never serialize onto any wire form.
	b, _ := json.Marshal(oai)
	if strings.Contains(string(b), "ProtocolExtensions") || strings.Contains(string(b), "anthropic-beta") {
		t.Fatal("envelope must stay out of wire encodings")
	}
}
