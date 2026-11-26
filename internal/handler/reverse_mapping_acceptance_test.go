package handler

// Reverse-mapping acceptance (2026-11-26, checklist §9.2): the REAL chain —
// admission (envelope-ization) → candidate eligibility (source-protocol
// aware) → AnthropicProvider against an httptest upstream — proving the
// user-visible outcome: an openai client's reasoning_effort reaches an
// anthropic upstream as output_config.effort, on the wire.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crosslink/internal/admission"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	"gorm.io/datatypes"
)

func TestReverseMapping_Acceptance_OpenAIToAnthropicWire(t *testing.T) {
	t.Parallel()
	body := `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`

	// 1. Admission: envelope-ization (production runs this in the pre-cache
	// middleware; the handler attaches the envelope to the request).
	env, err := admission.Admit("openai", []byte(body), "")
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	var req domain.OpenAIRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	req.Extensions = env

	// 2. Eligibility on a real-ish anthropic route (opted in, 2026-10-21).
	anthropicRow := &model.Provider{
		AdapterType: "anthropic",
		ExtraConfig: datatypes.JSON(`{"api_version":"2026-10-21"}`),
	}
	notOptedRow := &model.Provider{
		AdapterType: "anthropic",
		ExtraConfig: datatypes.JSON(`{"api_version":"2026-10-21"}`),
	}
	routes := []*router.RouteResult{
		{ProviderRow: notOptedRow, ProviderModel: "m-legacy"}, // cross-protocol → gated
	}
	if _, skips := service.FilterEffortEligibleRoutes(routes, req.Extensions); len(skips) != 1 {
		t.Fatalf("non-opted anthropic route must be skipped (cross-protocol gate), skips=%v", skips)
	}

	// 3. Real adapter against an httptest upstream — capture the wire.
	var gotBody map[string]any
	var gotVersion string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("anthropic-version")
		raw := make([]byte, r.ContentLength)
		n, _ := r.Body.Read(raw)
		_ = json.Unmarshal(raw[:n], &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "m1", "type": "message", "role": "assistant", "model": "m",
			"content":     []any{map[string]any{"type": "text", "text": "ok"}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	defer up.Close()

	p, err := provider.NewAnthropicProvider("an", up.URL, "k", []byte(`{"api_version":"2026-10-21"}`), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	opted := &router.RouteResult{
		Provider:          p,
		ProviderRow:       anthropicRow,
		ProviderModel:     "m-up",
		AllowedExtensions: []string{"output_config"},
	}
	eligible, skips := service.FilterEffortEligibleRoutes([]*router.RouteResult{opted}, req.Extensions)
	if len(eligible) != 1 || len(skips) != 0 {
		t.Fatalf("opted anthropic route must be eligible: %v %v", eligible, skips)
	}
	req.Model = opted.ProviderModel
	if _, err := p.Chat(context.Background(), &req, "k"); err != nil {
		t.Fatal(err)
	}

	// 4. The acceptance assertions.
	if gotVersion != "2026-10-21" {
		t.Fatalf("anthropic-version = %q", gotVersion)
	}
	oc, ok := gotBody["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("upstream body missing output_config: %v", gotBody)
	}
	if oc["effort"] != "high" {
		t.Fatalf("output_config.effort = %v, want high", oc["effort"])
	}
	if _, leaked := gotBody["reasoning_effort"]; leaked {
		t.Fatal("openai-native reasoning_effort must NOT appear on the anthropic wire")
	}
}
