package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/require"
)

// anthropicSSEUpstream serves one raw SSE payload then closes the stream.
func anthropicSSEUpstream(t *testing.T, payload string) *httptest.Server {
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

// Regression: an upstream `event: error` frame (or a mid-stream drop without
// message_stop) must NOT produce a Done terminal — the truncation has to stay
// visible to the service layer (which emits stream_interrupted to the client).
// A clean stream ending in message_stop must produce exactly one Done.
func TestAnthropicProvider_StreamChat_TerminalSemantics(t *testing.T) {
	cleanStream := "event: message_start\n" + `data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":3}}}` + "\n\n" +
		"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

	errorThenStop := "event: message_start\n" + `data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":3}}}` + "\n\n" +
		"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}` + "\n\n" +
		"event: error\n" + `data: {"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

	dropMidStream := "event: message_start\n" + `data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":3}}}` + "\n\n" +
		"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"par"}}` + "\n\n"

	t.Run("clean stream yields Done terminal", func(t *testing.T) {
		server := anthropicSSEUpstream(t, cleanStream)
		p, err := NewAnthropicProvider("anthropic", server.URL, "k", nil, 10*time.Second)
		require.NoError(t, err)
		req := &domain.OpenAIRequest{MaxTokens: intPtr(100), Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}
		ch, err := p.StreamChat(context.Background(), req, "k")
		require.NoError(t, err)
		doneCount := 0
		for chunk := range ch {
			if chunk.Done {
				doneCount++
			}
		}
		require.Equal(t, 1, doneCount, "clean anthropic stream must report exactly one Done terminal")
	})

	t.Run("upstream error frame poisons: no Done even with later message_stop", func(t *testing.T) {
		server := anthropicSSEUpstream(t, errorThenStop)
		p, err := NewAnthropicProvider("anthropic", server.URL, "k", nil, 10*time.Second)
		require.NoError(t, err)
		req := &domain.OpenAIRequest{MaxTokens: intPtr(100), Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}
		ch, err := p.StreamChat(context.Background(), req, "k")
		require.NoError(t, err)
		doneCount := 0
		text := ""
		for chunk := range ch {
			if chunk.Done {
				doneCount++
			}
			if chunk.Chunk != nil && len(chunk.Chunk.Choices) > 0 {
				text += chunk.Chunk.Choices[0].Delta.Content
			}
		}
		require.Equal(t, 0, doneCount, "error frame must not be laundered into a clean completion")
		require.Equal(t, "partial", text, "pre-error content must still be delivered")
	})

	t.Run("mid-stream drop without message_stop yields no Done", func(t *testing.T) {
		server := anthropicSSEUpstream(t, dropMidStream)
		p, err := NewAnthropicProvider("anthropic", server.URL, "k", nil, 10*time.Second)
		require.NoError(t, err)
		req := &domain.OpenAIRequest{MaxTokens: intPtr(100), Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}
		ch, err := p.StreamChat(context.Background(), req, "k")
		require.NoError(t, err)
		doneCount := 0
		for chunk := range ch {
			if chunk.Done {
				doneCount++
			}
		}
		require.Equal(t, 0, doneCount, "truncated anthropic stream must not report Done")
	})
}
