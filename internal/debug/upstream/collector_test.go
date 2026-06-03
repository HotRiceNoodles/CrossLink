package upstream

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCollector_AddAndCalls(t *testing.T) {
	c := &UpstreamCollector{}
	c.Add(UpstreamCall{Provider: "openai", StatusCode: 200})
	c.Add(UpstreamCall{Provider: "anthropic", StatusCode: 429})

	calls := c.Calls()
	assert.Len(t, calls, 2)
	assert.Equal(t, "openai", calls[0].Provider)
	assert.Equal(t, 200, calls[0].StatusCode)
	assert.Equal(t, "anthropic", calls[1].Provider)
	assert.Equal(t, 429, calls[1].StatusCode)
}

func TestCollector_CallsReturnsSnapshot(t *testing.T) {
	c := &UpstreamCollector{}
	c.Add(UpstreamCall{Provider: "first"})

	snapshot := c.Calls()
	c.Add(UpstreamCall{Provider: "second"})

	assert.Len(t, snapshot, 1, "snapshot should not reflect later additions")
	assert.Equal(t, "first", snapshot[0].Provider)
	assert.Len(t, c.Calls(), 2)
}

func TestCollector_ConcurrentAdd(t *testing.T) {
	c := &UpstreamCollector{}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				c.Add(UpstreamCall{Provider: "p"})
			}
		}()
	}
	wg.Wait()

	assert.Len(t, c.Calls(), 100)
}

func TestCollector_ByteBudgetEnforcement(t *testing.T) {
	c := &UpstreamCollector{}

	// First call: just under the limit
	body := make([]byte, UpstreamTotalLimit-1)
	c.Add(UpstreamCall{
		Provider:  "first",
		ReqBody:   body,
		StatusCode: 200,
	})
	calls := c.Calls()
	assert.Len(t, calls, 1)
	assert.NotNil(t, calls[0].ReqBody, "first call body should be kept")

	// Second call: pushes over the limit → bodies cleared, metadata preserved
	c.Add(UpstreamCall{
		Provider:   "second",
		ReqBody:    []byte("data"),
		RespBody:   []byte("resp"),
		StatusCode: 500,
		Method:     "POST",
	})
	calls = c.Calls()
	assert.Len(t, calls, 2)
	assert.Equal(t, "second", calls[1].Provider, "metadata should be preserved")
	assert.Equal(t, 500, calls[1].StatusCode)
	assert.Equal(t, "POST", calls[1].Method)
	assert.Nil(t, calls[1].ReqBody, "body should be cleared when over budget")
	assert.Nil(t, calls[1].RespBody, "body should be cleared when over budget")
}

func TestRedactHeaders_RemovesSensitive(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer secret")
	h.Set("X-Api-Key", "key123")
	h.Set("Cookie", "session=abc")
	h.Set("Set-Cookie", "token=xyz")
	h.Set("Proxy-Authorization", "Basic abc")
	h.Set("Content-Type", "application/json")

	redacted := RedactHeaders(h)

	assert.Empty(t, redacted.Get("Authorization"))
	assert.Empty(t, redacted.Get("X-Api-Key"))
	assert.Empty(t, redacted.Get("Cookie"))
	assert.Empty(t, redacted.Get("Set-Cookie"))
	assert.Empty(t, redacted.Get("Proxy-Authorization"))
	assert.Equal(t, "application/json", redacted.Get("Content-Type"))
}

func TestRedactHeaders_DoesNotMutateOriginal(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer secret")

	RedactHeaders(h)

	assert.Equal(t, "Bearer secret", h.Get("Authorization"), "original should not be modified")
}

func TestWithFallback_RoundTrip(t *testing.T) {
	ctx := context.Background()
	assert.False(t, IsFallbackFromContext(ctx))

	ctx = WithFallback(ctx, true)
	assert.True(t, IsFallbackFromContext(ctx))

	ctx = WithFallback(ctx, false)
	assert.False(t, IsFallbackFromContext(ctx))
}

func TestWithProviderName_RoundTrip(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, "", ProviderNameFromContext(ctx))

	ctx = WithProviderName(ctx, "openai_compatible")
	assert.Equal(t, "openai_compatible", ProviderNameFromContext(ctx))
}

func TestWithProviderModel_RoundTrip(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, "", ProviderModelFromContext(ctx))

	ctx = WithProviderModel(ctx, "gpt-4o")
	assert.Equal(t, "gpt-4o", ProviderModelFromContext(ctx))
}

func TestWithProviderBaseURL_RoundTrip(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, "", ProviderBaseURLFromContext(ctx))

	ctx = WithProviderBaseURL(ctx, "https://api.openai.com")
	assert.Equal(t, "https://api.openai.com", ProviderBaseURLFromContext(ctx))
}

func TestWithUpstreamCollector_RoundTrip(t *testing.T) {
	ctx := context.Background()
	assert.Nil(t, UpstreamCollectorFromContext(ctx))

	c := &UpstreamCollector{}
	ctx = WithUpstreamCollector(ctx, c)
	assert.Same(t, c, UpstreamCollectorFromContext(ctx))
}
