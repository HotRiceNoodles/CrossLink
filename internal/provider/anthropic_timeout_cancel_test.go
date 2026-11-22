package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
)

func minimalChatReq() *domain.OpenAIRequest {
	mt := 32
	return &domain.OpenAIRequest{
		Model: "m", MaxTokens: &mt,
		Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}},
	}
}

// M4 acceptance gaps: assert the constructor wires its timeout parameter into
// http.Client.Timeout, and that a slow upstream is actually CANCELLED by it
// (shortenable construction parameter, per the plan's acceptance wording).

func TestAnthropicProviderTimeoutWiring(t *testing.T) {
	t.Parallel()
	p, err := NewAnthropicProvider("t", "https://api.example.com", "k", nil, 250*time.Millisecond)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if p.httpClient.Timeout != 250*time.Millisecond {
		t.Fatalf("httpClient.Timeout = %v, want 250ms (M4: the value is the non-stream client TOTAL timeout)", p.httpClient.Timeout)
	}
	if p.streamClient.Timeout != 0 {
		t.Fatalf("streamClient.Timeout = %v, want 0 (streaming must not inherit the non-stream total timeout)", p.streamClient.Timeout)
	}
}

func TestAnthropicProviderSlowUpstreamCancelled(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	p, err := NewAnthropicProvider("slow", srv.URL, "k", nil, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	start := time.Now()
	_, err = p.Chat(context.Background(), minimalChatReq(), "k")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("slow upstream must be cancelled by the client timeout")
	}
	if elapsed >= 450*time.Millisecond {
		t.Fatalf("cancellation took %v; the 100ms total timeout must abort the call, not wait out the 500ms handler", elapsed)
	}
	// http.Client.Timeout surfaces as context.DeadlineExceeded wrapped in
	// *url.Error — ClassifyError deliberately maps that to "" (treated as a
	// caller-side deadline, non-retryable), so assert the raw cause instead.
	if !strings.Contains(err.Error(), "context deadline exceeded") &&
		!strings.Contains(err.Error(), "Client.Timeout") &&
		ClassifyError(err) != ErrorTimeout {
		t.Fatalf("error %q is not a client-timeout cancellation", err)
	}
}
