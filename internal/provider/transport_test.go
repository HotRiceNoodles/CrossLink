package provider

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crosslink/internal/debug/upstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDebugTransport_PassthroughWhenCollectorNil(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	dt := &debugTransport{base: http.DefaultTransport}
	req := httptest.NewRequest("GET", server.URL+"/test", nil)

	resp, err := dt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "ok", string(body))
	assert.Equal(t, 200, resp.StatusCode)
}

func TestDebugTransport_NonStreamCapture(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom", "visible")
		w.WriteHeader(200)
		w.Write([]byte(`{"result":"hello"}`))
	}))
	defer server.Close()

	collector := &upstream.UpstreamCollector{}
	ctx := context.Background()
	ctx = upstream.WithUpstreamCollector(ctx, collector)
	ctx = upstream.WithProviderName(ctx, "test-provider")
	ctx = upstream.WithProviderModel(ctx, "gpt-4")
	ctx = upstream.WithProviderBaseURL(ctx, "http://test.local")

	dt := &debugTransport{base: http.DefaultTransport}
	req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/chat/completions", strings.NewReader(`{"prompt":"hi"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer secret-key")

	resp, err := dt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Caller gets the full unmodified response body
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, `{"result":"hello"}`, string(body))

	// Collector captured the call
	calls := collector.Calls()
	require.Len(t, calls, 1)

	call := calls[0]
	assert.Equal(t, "test-provider", call.Provider)
	assert.Equal(t, "gpt-4", call.Model)
	assert.Contains(t, call.BaseURL, "127.0.0.1")
	assert.Equal(t, "POST", call.Method)
	assert.Equal(t, "/v1/chat/completions", call.Path)
	assert.Equal(t, 200, call.StatusCode)
	assert.True(t, call.Duration > 0)
	assert.Empty(t, call.Error)
	assert.Contains(t, string(call.ReqBody), `{"prompt":"hi"}`)
	assert.Contains(t, string(call.RespBody), `{"result":"hello"}`)

	// Sensitive headers redacted
	assert.Empty(t, call.ReqHeaders.Get("Authorization"))
	// Non-sensitive preserved
	assert.Equal(t, "visible", call.RespHeaders.Get("X-Custom"))
}

func TestDebugTransport_StreamCapture(t *testing.T) {
	sseData := "data: {\"chunk\":1}\n\ndata: {\"chunk\":2}\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(sseData))
	}))
	defer server.Close()

	collector := &upstream.UpstreamCollector{}
	ctx := upstream.WithUpstreamCollector(context.Background(), collector)

	dt := &debugTransport{base: http.DefaultTransport}
	req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/stream", nil)
	require.NoError(t, err)

	resp, err := dt.RoundTrip(req)
	require.NoError(t, err)

	// Read the stream body (simulating the caller consuming SSE)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), `{"chunk":1}`)

	// Close triggers the onClose callback → collector.Add
	resp.Body.Close()

	calls := collector.Calls()
	require.Len(t, calls, 1)
	assert.Contains(t, string(calls[0].RespBody), `{"chunk":1}`)
	assert.Equal(t, 200, calls[0].StatusCode)
}

func TestDebugTransport_BodyTruncation(t *testing.T) {
	// Response body larger than UpstreamBodyLimit
	bigResp := bytes.Repeat([]byte("x"), upstream.UpstreamBodyLimit+1024)
	bigReq := bytes.Repeat([]byte("y"), upstream.UpstreamBodyLimit+512)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify caller gets the full request body
		got, _ := io.ReadAll(r.Body)
		assert.Len(t, got, upstream.UpstreamBodyLimit+512)

		w.WriteHeader(200)
		w.Write(bigResp)
	}))
	defer server.Close()

	collector := &upstream.UpstreamCollector{}
	ctx := upstream.WithUpstreamCollector(context.Background(), collector)

	dt := &debugTransport{base: http.DefaultTransport}
	req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/big", bytes.NewReader(bigReq))
	require.NoError(t, err)

	resp, err := dt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Caller gets the full response body
	body, _ := io.ReadAll(resp.Body)
	assert.Len(t, body, upstream.UpstreamBodyLimit+1024, "caller should get full response")

	calls := collector.Calls()
	require.Len(t, calls, 1)
	assert.Len(t, calls[0].ReqBody, upstream.UpstreamBodyLimit, "req body should be truncated")
	assert.Len(t, calls[0].RespBody, upstream.UpstreamBodyLimit, "resp body should be truncated")
}

func TestDebugTransport_ErrorCapture(t *testing.T) {
	// Create and immediately close server to force connection error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := server.Listener.Addr().String()
	server.Close()

	collector := &upstream.UpstreamCollector{}
	ctx := upstream.WithUpstreamCollector(context.Background(), collector)

	dt := &debugTransport{base: http.DefaultTransport}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+addr+"/fail", nil)
	require.NoError(t, err)

	resp, err := dt.RoundTrip(req)
	assert.Error(t, err)
	assert.Nil(t, resp)

	calls := collector.Calls()
	require.Len(t, calls, 1)
	assert.NotEmpty(t, calls[0].Error)
	assert.Equal(t, 0, calls[0].StatusCode)
}

func TestDebugTransport_ContextMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	collector := &upstream.UpstreamCollector{}
	ctx := context.Background()
	ctx = upstream.WithUpstreamCollector(ctx, collector)
	ctx = upstream.WithProviderName(ctx, "anthropic")
	ctx = upstream.WithProviderModel(ctx, "claude-sonnet")
	ctx = upstream.WithProviderBaseURL(ctx, "https://api.anthropic.com")
	ctx = upstream.WithFallback(ctx, true)
	ctx = context.WithValue(ctx, retryAttemptKey{}, 3)

	dt := &debugTransport{base: http.DefaultTransport}
	req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/messages", nil)
	require.NoError(t, err)

	resp, err := dt.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()

	calls := collector.Calls()
	require.Len(t, calls, 1)
	call := calls[0]
	assert.Equal(t, "anthropic", call.Provider)
	assert.Equal(t, "claude-sonnet", call.Model)
	assert.Contains(t, call.BaseURL, "127.0.0.1") // BaseURL comes from req.URL, not context
	assert.Equal(t, 3, call.Attempt)
	assert.True(t, call.IsRetry)
	assert.True(t, call.IsFallback)
}

func TestDebugTransport_HeaderRedaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Authorization", "Bearer server-secret")
		w.Header().Set("X-Custom", "visible")
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	collector := &upstream.UpstreamCollector{}
	ctx := upstream.WithUpstreamCollector(context.Background(), collector)

	dt := &debugTransport{base: http.DefaultTransport}
	req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/test", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("X-Api-Key", "my-key")

	resp, err := dt.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()

	calls := collector.Calls()
	require.Len(t, calls, 1)

	// Request headers: sensitive removed
	assert.Empty(t, calls[0].ReqHeaders.Get("Authorization"))
	assert.Empty(t, calls[0].ReqHeaders.Get("X-Api-Key"))

	// Response headers: sensitive removed, non-sensitive preserved
	assert.Empty(t, calls[0].RespHeaders.Get("Authorization"))
	assert.Equal(t, "visible", calls[0].RespHeaders.Get("X-Custom"))
}
