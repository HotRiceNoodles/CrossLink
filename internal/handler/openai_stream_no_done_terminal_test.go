package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// End-to-end chain tests for the MiniMax-style upstream: a REAL HTTP SSE mock
// upstream -> REAL OpenAICompatibleProvider (network) -> OpenAIHandler stream
// path -> recorded client response. They pin the client-visible contract of
// the terminal-frame tolerance fix:
//   - clean EOF + all choices finished (no [DONE]) => normal completion:
//     exactly one `data: [DONE]`, NO stream_interrupted, content/tool args/
//     usage intact;
//   - truncated EOF (missing finish_reason) => stream_interrupted still sent;
//   - strict guardrail still buffers and final-checks before releasing, for
//     both synthesized and sentinel terminations.

func chunkLine(jsonPayload string) string { return "data: " + jsonPayload + "\n\n" }

func finishChunk(reason string) string {
	return fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":%q}]}`, reason)
}

func contentChunk(text string) string {
	return fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, text)
}

// runStreamOverHTTP spins a real SSE upstream, a real provider pointed at it,
// and drives OpenAIHandler.handleStream to completion, returning the client
// response body, the gin context (for usage assertions) and the number of
// upstream HTTP requests actually issued.
func runStreamOverHTTP(t *testing.T, ssePayload string, gs *guardrail.GuardrailService) (string, *gin.Context, int32) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var upstreamCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(ssePayload))
		flusher.Flush()
	}))
	defer server.Close()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.UsageLog{}))
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}

	h := &OpenAIHandler{
		guardrailSvc: gs,
		usageSvc:     service.NewUsageService(repository.NewUsageLogRepo(db)),
	}

	p := provider.NewOpenAICompatibleWithTransport("mockup", server.URL, 10*time.Second, &http.Transport{})
	routes := []*router.RouteResult{{
		Provider:      p,
		ProviderModel: "mock-model",
		ProviderRow:   &model.Provider{},
	}}

	req := &domain.OpenAIRequest{
		Stream:   true,
		Model:    "mock-model",
		Messages: []domain.OpenAIMessage{{Role: "user", Content: "hello"}},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	h.handleStream(c, routes, req, nil, 0, time.Now(), "test-session")
	return w.Body.String(), c, atomic.LoadInt32(&upstreamCalls)
}

func TestHandleStream_NoDONE_AllFinished_CompletesNormally(t *testing.T) {
	payload := chunkLine(contentChunk("Hello")) +
		chunkLine(contentChunk(" world")) +
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":5,"total_tokens":14}}` + "\n\n"

	body, c, calls := runStreamOverHTTP(t, payload, nil)

	assert.NotContains(t, body, "stream_interrupted",
		"clean EOF with finish_reason must complete normally, not report interruption")
	assert.Equal(t, 1, strings.Count(body, "data: [DONE]"), "client must see exactly one terminal [DONE]")
	assert.Contains(t, body, `"finish_reason":"stop"`, "finish_reason chunk must be forwarded to the client")
	assert.Contains(t, body, "Hello", "content must reach the client")
	assert.Contains(t, body, `"completion_tokens":5`, "usage from the finish chunk must reach the client")

	ot, ok := c.Get("output_tokens")
	require.True(t, ok, "output tokens must be published for usage accounting")
	assert.Equal(t, 5, ot.(int), "provider-reported usage must win over estimation")
	assert.Equal(t, int32(1), calls, "upstream must be executed exactly once — terminal synthesis must not re-request")
}

func TestHandleStream_NoDONE_Truncated_StillEmitsStreamInterrupted(t *testing.T) {
	payload := chunkLine(contentChunk("partial answer")) // no finish_reason, no [DONE]

	body, c, calls := runStreamOverHTTP(t, payload, nil)

	assert.Contains(t, body, "stream_interrupted", "EOF without finish_reason must still be reported as an interruption")
	assert.Contains(t, body, "upstream stream ended unexpectedly")
	assert.Equal(t, 1, strings.Count(body, "data: [DONE]"))
	assert.Contains(t, body, "partial answer")
	assert.Equal(t, int32(1), calls, "interrupted stream must not silently re-execute the upstream")

	// C6-closure regression: truncation path must still publish token usage.
	_, ok := c.Get("input_tokens")
	assert.True(t, ok, "input_tokens must be published even on interrupted streams")
}

func TestHandleStream_NoDONE_ToolCallArgsCompleteAndSingleTerminal(t *testing.T) {
	header := `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`
	frag1 := `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}`
	frag2 := `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Osaka\"}"}}]},"finish_reason":null}]}`
	payload := chunkLine(header) + chunkLine(frag1) + chunkLine(frag2) + chunkLine(finishChunk("tool_calls"))

	body, _, _ := runStreamOverHTTP(t, payload, nil)

	assert.NotContains(t, body, "stream_interrupted")
	assert.Equal(t, 1, strings.Count(body, "data: [DONE]"))

	// Reassemble the tool call from the client-visible chunks and verify the
	// fragments were forwarded exactly once, intact.
	var args strings.Builder
	nameCount := 0
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: {") || strings.HasPrefix(line, "data: [") {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			for _, tc := range c.Delta.ToolCalls {
				if tc.Function.Name != "" {
					nameCount++
				}
				args.WriteString(tc.Function.Arguments)
			}
		}
	}
	assert.Equal(t, `{"city":"Osaka"}`, args.String(), "tool-call arguments must arrive complete and unduplicated")
	assert.Equal(t, 1, nameCount, "tool header must be emitted exactly once")
}

// newStrictGuardrail builds an enabled strict-mode guardrail service over a
// real sqlite DB, optionally seeded with one enabled keyword rule.
func newStrictGuardrail(t *testing.T, blocklist []string) *guardrail.GuardrailService {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	// sqlite-compatible DDL mirroring the production migrations (GORM
	// AutoMigrate chokes on the model's jsonb/not-null-now() tags — same
	// pattern as internal/guardrail/m3_reload_test.go).
	require.NoError(t, db.Exec(`CREATE TABLE guardrail_rules (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		org_id INTEGER,
		name TEXT NOT NULL,
		type TEXT NOT NULL,
		direction TEXT NOT NULL,
		enabled INTEGER DEFAULT 1,
		config TEXT NOT NULL,
		severity TEXT DEFAULT 'medium',
		action TEXT DEFAULT 'block',
		model_filter TEXT,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		version INTEGER NOT NULL DEFAULT 1
	)`).Error)
	for i, kw := range blocklist {
		require.NoError(t, db.Exec(
			`INSERT INTO guardrail_rules (name, type, direction, enabled, config, severity, action, version)
			 VALUES (?, 'keyword_filter', 'response', 1, ?, 'high', 'block', 1)`,
			fmt.Sprintf("rule-%d", i),
			fmt.Sprintf(`{"blocklist":["%s"],"allowlist":[],"use_regex":false,"case_sensitive":false}`, kw),
		).Error)
	}
	gs := guardrail.NewGuardrailService(db, nil)
	gs.SetEnabled(true)
	gs.SetStreamMode("strict")
	gs.SetFailOpen(false)
	return gs
}

func TestHandleStream_NoDONE_StrictGuardrail_Allowed_ReleasesAfterFinalCheck(t *testing.T) {
	gs := newStrictGuardrail(t, []string{"forbidden-token"})

	payload := chunkLine(contentChunk("a perfectly")) +
		chunkLine(contentChunk(" safe answer")) +
		chunkLine(finishChunk("stop"))

	body, _, _ := runStreamOverHTTP(t, payload, gs)

	assert.NotContains(t, body, "guardrail_blocked", "non-matching content must be released by the strict final check")
	assert.NotContains(t, body, "stream_interrupted")
	assert.Equal(t, 1, strings.Count(body, "data: [DONE]"))
	assert.Contains(t, body, "a perfectly", "checked content must be released after the synthesized terminal")
	assert.Contains(t, body, "safe answer")
}

func TestHandleStream_NoDONE_StrictGuardrail_Blocked_NeverReleases(t *testing.T) {
	gs := newStrictGuardrail(t, []string{"forbidden-token"})

	payload := chunkLine(contentChunk("this contains forbidden")) +
		chunkLine(contentChunk("-token inside")) +
		chunkLine(finishChunk("stop"))

	body, c, _ := runStreamOverHTTP(t, payload, gs)

	assert.Contains(t, body, "guardrail_blocked", "matching content must be blocked at the strict final check")
	assert.NotContains(t, body, "this contains forbidden", "blocked content must never be released")
	assert.Equal(t, 1, strings.Count(body, "data: [DONE]"), "blocked stream still terminates cleanly with one [DONE]")
	_, ok := c.Get("guardrail_triggered")
	assert.True(t, ok)
}
