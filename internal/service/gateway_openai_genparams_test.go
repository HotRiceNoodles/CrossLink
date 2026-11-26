package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/crosslink/internal/admission"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// P0 audit fix: wire-native generation params (response_format/logit_bias
// were hard-400'd by admission; penalties/seed/logprobs family silently
// dropped by the struct gate). Three proofs: admission passes them, they
// reach the upstream wire verbatim, and the logprobs RESPONSE side returns.

func TestGenParams_AdmissionPasses(t *testing.T) {
	t.Parallel()
	// The exact body that previously drew 400s (response_format, logit_bias)
	// and silent drops (the scalars).
	body := `{"model":"gptx","max_tokens":16,
		"response_format":{"type":"json_schema","schema":{"type":"object"},"strict":true},
		"frequency_penalty":0.3,"presence_penalty":0.2,"seed":42,
		"parallel_tool_calls":false,"logprobs":true,"top_logprobs":5,
		"logit_bias":{"50256":-100},"store":false,
		"service_tier":"auto",
		"messages":[{"role":"user","content":"hi"}]}`
	env, err := admission.Admit("openai", []byte(body), "")
	require.NoError(t, err, "the whole generation-param family must pass admission")
	// Nothing rides the envelope: the family are struct fields (service_tier
	// stays observe-dropped — billing-coupled, rides nowhere by design).
	// Edition-tolerant: the community build carries nothing (nil env); the
	// enterprise overlay registers additional dialect params (e.g.
	// anthropic.output_config) that legitimately ride the envelope. The
	// invariant under test is "the family passes admission" — asserted by
	// the NoError above — not the envelope's emptiness.
	_ = env

	// And the handler's struct parse keeps every field (the openai inbound
	// path unmarshals this same body into domain.OpenAIRequest).
	var req domain.OpenAIRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	require.NotNil(t, req.ResponseFormat)
	require.NotNil(t, req.FrequencyPenalty)
	require.NotNil(t, req.PresencePenalty)
	require.NotNil(t, req.Seed)
	require.NotNil(t, req.ParallelToolCalls)
	require.NotNil(t, req.Logprobs)
	require.NotNil(t, req.TopLogprobs)
	require.NotNil(t, req.LogitBias)
	require.NotNil(t, req.Store)
}

func TestGenParams_ReachUpstreamWire_Verbatim(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var wire map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		mu.Lock()
		_ = json.Unmarshal(body, &wire)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop","logprobs":{"content":[{"token":"ok","logprob":-0.1}]}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`)
	}))
	t.Cleanup(srv.Close)

	p := provider.NewOpenAICompatible("oa", srv.URL, 5*time.Second)
	req := &domain.OpenAIRequest{
		Model:             "gptx",
		MaxTokens:         i64p(16),
		Messages:          []domain.OpenAIMessage{{Role: "user", Content: "hi"}},
		ResponseFormat:    json.RawMessage(`{"type":"json_schema","schema":{"type":"object"},"strict":true}`),
		FrequencyPenalty:  f64(0.3),
		PresencePenalty:   f64(0.2),
		Seed:              i64p(42),
		ParallelToolCalls: boolp(false),
		Logprobs:          boolp(true),
		TopLogprobs:       i64p(5),
		LogitBias:         json.RawMessage(`{"50256":-100}`),
		Store:             boolp(false),
	}
	resp, err := p.Chat(context.Background(), req, "k")
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.NotNil(t, wire)
	assert.Equal(t, map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}, "strict": true}, wire["response_format"])
	assert.Equal(t, 0.3, wire["frequency_penalty"])
	assert.Equal(t, 0.2, wire["presence_penalty"])
	assert.Equal(t, 42.0, wire["seed"])
	assert.Equal(t, false, wire["parallel_tool_calls"])
	assert.Equal(t, true, wire["logprobs"])
	assert.Equal(t, 5.0, wire["top_logprobs"])
	assert.Equal(t, map[string]any{"50256": -100.0}, wire["logit_bias"])
	assert.Equal(t, false, wire["store"])
	assert.NotContains(t, wire, "service_tier")

	// Response side: logprobs rides back on the choice.
	require.Len(t, resp.Choices, 1)
	assert.NotNil(t, resp.Choices[0].Logprobs, "logprobs response must pass back openai→openai")
	var lp struct {
		Content []struct {
			Token   string  `json:"token"`
			Logprob float64 `json:"logprob"`
		} `json:"content"`
	}
	require.NoError(t, json.Unmarshal(resp.Choices[0].Logprobs, &lp))
	require.Len(t, lp.Content, 1)
	assert.Equal(t, "ok", lp.Content[0].Token)
}

func boolp(v bool) *bool { return &v }
