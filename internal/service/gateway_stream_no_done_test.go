package service

import (
	"context"
	"fmt"
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
)

// Messages-path regression for the MiniMax-style upstream: a REAL HTTP SSE
// mock upstream (no `data: [DONE]`) -> REAL OpenAICompatibleProvider ->
// GatewayService.StreamChatWithConnect -> Anthropic-SSE events. The Anthropic
// client path derives its terminal from finish_reason, so it must complete
// with exactly one message_stop — and the provider-side synthesized terminal
// (P0 fix) must not introduce a SECOND message_stop via the disconnect
// fallback (StreamTranslator stays stateDone, OnProviderDisconnect emits
// nothing). The provider uses the transport-injection constructor with a
// plain transport so no global SSRF state is touched.

func noDONESSEServer(t *testing.T, payload string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(payload))
		flusher.Flush()
	}))
	t.Cleanup(server.Close)
	return server
}

func runMessagesStream(t *testing.T, ssePayload string) []StreamEvent {
	t.Helper()
	server := noDONESSEServer(t, ssePayload)

	reg := provider.NewRegistry()
	reg.Register("mockup", provider.NewOpenAICompatibleWithTransport("mockup", server.URL, 10*time.Second, &http.Transport{}))

	repo := &mockRepo{data: map[string][]model.ProviderModel{
		"claude-3": {{
			ID: 1, ProviderModel: "mock-model", Weight: 1, Status: 1,
			Provider: model.Provider{Name: "mockup", Status: 1, ID: 1},
		}},
	}}
	resolver := router.NewResolver(reg, repo, nil, map[router.StrategyName]router.RoutingStrategy{
		router.StrategyWeightedRandom: &router.WeightedRandomStrategy{},
	}, nil, nil, nil, nil)
	svc := NewGatewayService(resolver, reg, nil, nil, nil)

	var mu sync.Mutex
	var events []StreamEvent
	_, err := svc.StreamChatWithConnect(context.Background(),
		&domain.AnthropicRequest{Model: "claude-3", MaxTokens: 100, Messages: makeMessages("hi")},
		func(_ context.Context, ev StreamEvent) bool {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
			return true
		}, nil, "", 0)
	if err != nil {
		t.Fatalf("StreamChatWithConnect: %v", err)
	}
	return events
}

func countEvents(events []StreamEvent, name string) int {
	n := 0
	for _, e := range events {
		if e.Event == name {
			n++
		}
	}
	return n
}

func TestMessagesPath_NoDONE_FinishStop_SingleMessageStop(t *testing.T) {
	payload := "data: " + `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}` + "\n\n" +
		"data: " + `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}` + "\n\n" +
		"data: " + `{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n\n"

	events := runMessagesStream(t, payload)

	if got := countEvents(events, "message_start"); got != 1 {
		t.Fatalf("message_start = %d, want 1", got)
	}
	if got := countEvents(events, "message_stop"); got != 1 {
		t.Fatalf("message_stop = %d, want exactly 1 (finish_reason terminal + no duplicate from disconnect fallback)", got)
	}
	var text strings.Builder
	for _, e := range events {
		if e.Event == "content_block_delta" && strings.Contains(e.Data, "text_delta") {
			// crude but sufficient: pull "text":"..." fragments
			const marker = `"text":"`
			i := strings.Index(e.Data, marker)
			if i >= 0 {
				rest := e.Data[i+len(marker):]
				if j := strings.Index(rest, `"`); j >= 0 {
					text.WriteString(rest[:j])
				}
			}
		}
	}
	if text.String() != "Hello" {
		t.Fatalf("reassembled text = %q, want %q", text.String(), "Hello")
	}
	// stop -> end_turn mapping must survive the synthesized-terminal path.
	foundStopReason := false
	for _, e := range events {
		if e.Event == "message_delta" && strings.Contains(e.Data, `"stop_reason":"end_turn"`) {
			foundStopReason = true
		}
	}
	if !foundStopReason {
		t.Fatal("message_delta with stop_reason=end_turn missing")
	}
}

func TestMessagesPath_NoDONE_FinishToolCalls_SingleMessageStop(t *testing.T) {
	payload := "data: " + `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}` + "\n\n" +
		"data: " + `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"Osaka\"}"}}]},"finish_reason":null}]}` + "\n\n" +
		"data: " + `{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"

	events := runMessagesStream(t, payload)

	if got := countEvents(events, "message_stop"); got != 1 {
		t.Fatalf("message_stop = %d, want exactly 1", got)
	}
	joined := ""
	for _, e := range events {
		joined += e.Data
	}
	if !strings.Contains(joined, "get_weather") || !strings.Contains(joined, "Osaka") {
		t.Fatal("tool call name/arguments lost on the no-[DONE] synthesized-terminal path")
	}
	foundToolUse := false
	for _, e := range events {
		if e.Event == "message_delta" && strings.Contains(e.Data, `"stop_reason":"tool_use"`) {
			foundToolUse = true
		}
	}
	if !foundToolUse {
		t.Fatal("message_delta with stop_reason=tool_use missing")
	}
}

func TestMessagesPath_StandardDONE_Regression(t *testing.T) {
	payload := "data: " + fmt.Sprint(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`) + "\n\n" +
		"data: " + `{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	events := runMessagesStream(t, payload)

	if got := countEvents(events, "message_stop"); got != 1 {
		t.Fatalf("message_stop = %d, want exactly 1 on the standard [DONE] path", got)
	}
}
