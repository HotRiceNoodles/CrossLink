package provider

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file pin the terminal-frame tolerance contract for
// OpenAI-compatible upstreams that close the SSE stream without `data: [DONE]`
// (observed in the wild from MiniMax): a clean EOF is reclassified as normal
// completion ONLY when every choice that appeared carried an explicit
// finish_reason, and no cancellation, read error, malformed chunk, or upstream
// error frame was seen. Every other ending must NOT produce a synthesized
// terminal chunk — the stream stays interrupted for the caller's graceful
// degradation. Standard `data: [DONE]` behavior is unchanged.

// newSSEServer serves the given raw SSE payload once per request and closes
// the response cleanly (proper HTTP termination, no [DONE] unless the payload
// contains one).
func newSSEServer(t *testing.T, payload string) *httptest.Server {
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

func collectChunks(t *testing.T, ch <-chan domain.SSEChunk) []domain.SSEChunk {
	t.Helper()
	var chunks []domain.SSEChunk
	deadline := time.After(10 * time.Second)
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return chunks
			}
			chunks = append(chunks, c)
		case <-deadline:
			t.Fatal("stream channel did not close within 10s")
			return nil
		}
	}
}

func doneCount(chunks []domain.SSEChunk) int {
	n := 0
	for _, c := range chunks {
		if c.Done {
			n++
		}
	}
	return n
}

// streamFrom streams from the given server URL; n<=1 omits the n parameter
// (single-choice request), n>1 sets it so the reader expects that many choices.
func streamFrom(t *testing.T, serverURL string, n int) <-chan domain.SSEChunk {
	t.Helper()
	p := NewOpenAICompatible("mockup", serverURL, 10*time.Second)
	req := &domain.OpenAIRequest{Model: "m", Stream: true}
	if n > 1 {
		req.N = &n
	}
	ch, err := p.StreamChat(context.Background(), req, "key")
	require.NoError(t, err)
	return ch
}

// chunkLine builds one SSE data line from a raw JSON payload.
func chunkLine(jsonPayload string) string {
	return "data: " + jsonPayload + "\n\n"
}

const (
	roleChunk    = `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`
	contentHi    = `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`
	contentThere = `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":" there"},"finish_reason":null}]}`
)

func finishChunk(reason string) string {
	return fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":%q}]}`, reason)
}

func TestReadSSEStream_NoDONE_CleanEOF_AllFinished_SynthesizesTerminal(t *testing.T) {
	for _, reason := range []string{"stop", "length", "tool_calls"} {
		t.Run(reason, func(t *testing.T) {
			payload := chunkLine(roleChunk) + chunkLine(contentHi) + chunkLine(finishChunk(reason))
			chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 1))

			require.NotEmpty(t, chunks, "all content chunks must be forwarded")
			last := chunks[len(chunks)-1]
			assert.True(t, last.Done, "clean EOF with all choices finished must synthesize exactly one terminal")
			assert.Equal(t, 1, doneCount(chunks), "terminal must be synthesized exactly once")
			assert.Equal(t, "Hi", chunks[1].Chunk.Choices[0].Delta.Content)
			assert.Equal(t, reason, *chunks[len(chunks)-2].Chunk.Choices[0].FinishReason)
		})
	}
}

func TestReadSSEStream_NoDONE_UsageTailAfterFinish_ForwardedThenTerminal(t *testing.T) {
	// finish_reason chunk without usage, then a separate usage-tail chunk with
	// empty choices (the stream_options.include_usage shape), then clean EOF.
	finish := `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	usageTail := `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`
	payload := chunkLine(contentHi) + chunkLine(finish) + chunkLine(usageTail)

	chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 1))

	require.GreaterOrEqual(t, len(chunks), 3)
	assert.Equal(t, "stop", *chunks[1].Chunk.Choices[0].FinishReason)
	require.NotNil(t, chunks[2].Chunk.Usage, "usage tail chunk must still be forwarded after finish_reason")
	assert.Equal(t, 7, chunks[2].Chunk.Usage.CompletionTokens)
	assert.True(t, chunks[3].Done, "terminal synthesized after usage tail")
	assert.Equal(t, 1, doneCount(chunks))
}

func TestReadSSEStream_NoDONE_MultiChoice(t *testing.T) {
	choice := func(idx int, content, finish string) string {
		delta := fmt.Sprintf(`{"content":%q}`, content)
		fin := "null"
		if finish != "" {
			fin = fmt.Sprintf("%q", finish)
		}
		return fmt.Sprintf(`{"id":"c1","model":"m","choices":[{"index":%d,"delta":%s,"finish_reason":%s}]}`, idx, delta, fin)
	}

	t.Run("all choices finished (n=2) -> terminal", func(t *testing.T) {
		payload := chunkLine(choice(0, "a", "")) + chunkLine(choice(1, "b", "")) +
			chunkLine(choice(0, "", "stop")) + chunkLine(choice(1, "", "stop"))
		chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 2))
		assert.Equal(t, 1, doneCount(chunks), "both expected choices finished: clean EOF must synthesize terminal")
		assert.True(t, chunks[len(chunks)-1].Done)
	})

	t.Run("partial finish (n=2, choice 1 seen but unfinished) -> no terminal", func(t *testing.T) {
		payload := chunkLine(choice(0, "a", "")) + chunkLine(choice(1, "b", "")) +
			chunkLine(choice(0, "", "stop")) // choice 1 never reports finish_reason
		chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 2))
		assert.Equal(t, 0, doneCount(chunks), "a choice without finish_reason makes the EOF a truncation, not completion")
	})

	t.Run("expected choice completely missing (n=2, only choice 0 ever sent) -> no terminal", func(t *testing.T) {
		payload := chunkLine(choice(0, "a", "")) + chunkLine(choice(0, "", "stop"))
		chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 2))
		assert.Equal(t, 0, doneCount(chunks),
			"an n=2 request whose choice 1 never appeared must NOT be classified as completed")
	})

	t.Run("empty-string finish_reason does not count", func(t *testing.T) {
		payload := chunkLine(choice(0, "a", "")) +
			"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"\"}]}\n\n"
		chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 1))
		assert.Equal(t, 0, doneCount(chunks), "explicit finish_reason must be non-empty to count")
	})
}

func TestReadSSEStream_MalformedThenDONE_NoTerminal(t *testing.T) {
	payload := chunkLine(contentHi) + chunkLine(finishChunk("stop")) +
		"data: {not-json\n\n" + "data: [DONE]\n\n"
	chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 1))
	assert.Equal(t, 0, doneCount(chunks),
		"a [DONE] sentinel after a malformed chunk must not launder the stream into success")
}

func TestReadSSEStream_UpstreamErrorThenDONE_NoTerminal(t *testing.T) {
	payload := chunkLine(contentHi) + chunkLine(finishChunk("stop")) +
		chunkLine(`{"error":{"message":"boom","type":"server_error"}}`) + "data: [DONE]\n\n"
	chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 1))
	assert.Equal(t, 0, doneCount(chunks),
		"a [DONE] sentinel after an upstream error frame must not launder the stream into success")
}

func TestReadSSEStream_UpstreamErrorMessageNotLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	secret := "SECRET-UNSANITIZED-UPSTREAM-MESSAGE"
	payload := chunkLine(contentHi) + chunkLine(finishChunk("stop")) +
		chunkLine(fmt.Sprintf(`{"error":{"message":%q,"type":"server_error"}}`, secret))
	chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 1))
	assert.Equal(t, 0, doneCount(chunks))

	logged := buf.String()
	assert.Contains(t, logged, "upstream error frame in stream", "the error frame must still be logged")
	assert.Contains(t, logged, "type=server_error")
	assert.NotContains(t, logged, secret,
		"the raw upstream error message is untrusted content and must never reach logs unsanitized")
}

func TestReadSSEStream_MalformedPayloadBodyNotLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	secret := "SENSITIVE-MALFORMED-PAYLOAD-BODY"
	payload := chunkLine(contentHi) +
		"data: {\"choices\":" + secret + "\n\n" + // fails to unmarshal as OpenAIChunk
		chunkLine(finishChunk("stop"))
	chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 1))
	assert.Equal(t, 0, doneCount(chunks))

	logged := buf.String()
	assert.Contains(t, logged, "skipped malformed SSE chunk", "the malformed chunk must still be logged")
	assert.NotContains(t, logged, secret,
		"the malformed payload body is untrusted content and must never reach logs")
	assert.Contains(t, logged, "payload_bytes=")
}

func TestReadSSEStream_UpstreamErrorTypeClassifiedNotVerbatim(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	weird := "injected\ntype<bogus-category>"
	payload := chunkLine(contentHi) + chunkLine(finishChunk("stop")) +
		chunkLine(fmt.Sprintf(`{"error":{"type":%q}}`, weird))
	chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 1))
	assert.Equal(t, 0, doneCount(chunks))

	logged := buf.String()
	assert.Contains(t, logged, "type=unknown",
		"an untrusted upstream error type outside the known set must log as a fixed category")
	assert.NotContains(t, logged, "injected",
		"the raw untrusted type string must never reach logs verbatim")
}

func TestReadSSEStream_ErrorFrameWithChoices_NoTerminal(t *testing.T) {
	// Counter-case for the probe: an upstream may attach the error object to a
	// chunk that ALSO carries choices (e.g. the final finish chunk). The error
	// must still be detected regardless of the choices array.
	errWithChoices := `{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"error":{"type":"server_error","message":"boom"}}`

	t.Run("clean EOF after error-with-choices frame", func(t *testing.T) {
		payload := chunkLine(contentHi) + chunkLine(errWithChoices)
		chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 1))
		assert.Equal(t, 0, doneCount(chunks),
			"an error frame riding on a choices-carrying chunk must still disqualify completion")
	})

	t.Run("[DONE] after error-with-choices frame", func(t *testing.T) {
		payload := chunkLine(contentHi) + chunkLine(errWithChoices) + "data: [DONE]\n\n"
		chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 1))
		assert.Equal(t, 0, doneCount(chunks),
			"a [DONE] sentinel must not launder an error-with-choices frame into success")
	})
}

func TestReadSSEStream_Truncated_NoFinishReason_NoTerminal(t *testing.T) {
	payload := chunkLine(roleChunk) + chunkLine(contentHi) + chunkLine(contentThere)
	chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 1))
	assert.Equal(t, 3, len(chunks))
	assert.Equal(t, 0, doneCount(chunks), "EOF without any finish_reason must stay an interruption")
}

// rawTruncatedChunkedServer speaks HTTP/1.1 chunked encoding manually and
// closes the TCP connection mid-body (no terminating 0-length chunk), so the
// client's body reader surfaces a real read error (unexpected EOF) after the
// bytes already delivered.
func rawTruncatedChunkedServer(t *testing.T, ssePayload string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Drain the request head + body so the client request completes.
		br := bufio.NewReader(conn)
		var contentLen int
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(strings.ToLower(line), "content-length:") {
				fmt.Sscanf(line[len("Content-Length:"):], "%d", &contentLen)
			}
			if line == "\r\n" {
				io.CopyN(io.Discard, br, int64(contentLen))
				break
			}
		}
		head := "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n"
		conn.Write([]byte(head))
		// Send the whole SSE payload as one valid HTTP chunk...
		fmt.Fprintf(conn, "%x\r\n%s\r\n", len(ssePayload), ssePayload)
		// ...then vanish without the terminating zero chunk -> read error.
		conn.Close()
	}()

	return "http://" + ln.Addr().String()
}

func TestReadSSEStream_FinishThenReadError_NoTerminal(t *testing.T) {
	payload := chunkLine(contentHi) + chunkLine(finishChunk("stop"))
	chunks := collectChunks(t, streamFrom(t, rawTruncatedChunkedServer(t, payload), 1))
	assert.GreaterOrEqual(t, len(chunks), 1)
	assert.Equal(t, 0, doneCount(chunks),
		"a read error after finish_reason must NOT be upgraded to normal completion")
}

func TestReadSSEStream_FinishThenConsumerCancel_NoTerminal(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, chunkLine(contentHi))
		fmt.Fprint(w, chunkLine(finishChunk("stop")))
		flusher.Flush()
		<-release
	}))
	defer server.Close()
	defer close(release)

	p := NewOpenAICompatible("mockup", server.URL, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.StreamChat(ctx, &domain.OpenAIRequest{Model: "m", Stream: true}, "key")
	require.NoError(t, err)

	// Read through the finish_reason chunk, then cancel like a disconnecting
	// client before the stream terminates.
	for c := range ch {
		if c.Chunk != nil && c.Chunk.Choices != nil && c.Chunk.Choices[0].FinishReason != nil {
			break
		}
	}
	cancel()

	for c := range ch { // drain until the producer closes the channel
		if c.Done {
			t.Fatal("cancelled stream must never synthesize a terminal chunk")
		}
	}
}

func TestReadSSEStream_UpstreamErrorFrame_NoTerminal(t *testing.T) {
	errFrame := `{"error":{"message":"upstream overloaded","type":"server_error"}}`
	t.Run("error frame after finish_reason", func(t *testing.T) {
		payload := chunkLine(contentHi) + chunkLine(finishChunk("stop")) + chunkLine(errFrame)
		chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 1))
		assert.Equal(t, 0, doneCount(chunks), "an upstream error frame disqualifies clean-EOF completion")
	})
	t.Run("error frame without finish_reason", func(t *testing.T) {
		payload := chunkLine(contentHi) + chunkLine(errFrame)
		chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 1))
		assert.Equal(t, 0, doneCount(chunks))
	})
}

func TestReadSSEStream_EmptyStream_NoTerminal(t *testing.T) {
	chunks := collectChunks(t, streamFrom(t, newSSEServer(t, "").URL, 1))
	assert.Empty(t, chunks)
	// Zero chunks means no terminal was synthesized (an empty stream is not a
	// completion); doneCount over an empty slice is trivially 0.
	assert.Equal(t, 0, doneCount(chunks))
}

func TestReadSSEStream_MalformedChunk_NoTerminal(t *testing.T) {
	payload := chunkLine(contentHi) + "data: {not-json\n\n" + chunkLine(finishChunk("stop"))
	chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 1))
	assert.Equal(t, 0, doneCount(chunks), "any malformed SSE chunk keeps the EOF classified as interrupted")
}

func TestReadSSEStream_ToolCallFragmentsIntact_NoDONE(t *testing.T) {
	header := `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`
	frag1 := `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}`
	frag2 := `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Beijing\"}"}}]},"finish_reason":null}]}`
	payload := chunkLine(header) + chunkLine(frag1) + chunkLine(frag2) + chunkLine(finishChunk("tool_calls"))

	chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 1))

	require.GreaterOrEqual(t, len(chunks), 5)
	var args strings.Builder
	for _, c := range chunks {
		if c.Chunk == nil {
			continue
		}
		for _, ch := range c.Chunk.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				args.WriteString(tc.Function.Arguments)
			}
		}
	}
	assert.Equal(t, `{"city":"Beijing"}`, args.String(), "tool-call argument fragments must be forwarded intact")
	assert.Equal(t, 1, doneCount(chunks))
	assert.True(t, chunks[len(chunks)-1].Done)
}

func TestReadSSEStream_StandardDONE_Unchanged(t *testing.T) {
	payload := chunkLine(contentHi) + chunkLine(finishChunk("stop")) + "data: [DONE]\n\n"
	chunks := collectChunks(t, streamFrom(t, newSSEServer(t, payload).URL, 1))
	require.GreaterOrEqual(t, len(chunks), 3)
	assert.True(t, chunks[len(chunks)-1].Done, "explicit [DONE] still terminates the stream")
	assert.Equal(t, 1, doneCount(chunks), "exactly one terminal for a [DONE]-terminated stream")
}
