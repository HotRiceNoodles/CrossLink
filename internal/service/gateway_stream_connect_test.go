package service

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
)

// streamFailProvider fails its stream connection so the engine falls back.
type streamFailProvider struct{ name string }

func (p *streamFailProvider) Chat(context.Context, *domain.OpenAIRequest, string) (*domain.OpenAIResponse, error) {
	return nil, nil
}
func (p *streamFailProvider) StreamChat(context.Context, *domain.OpenAIRequest, string) (<-chan domain.SSEChunk, error) {
	return nil, &provider.ProviderError{StatusCode: 500, ErrorType: provider.ErrorServer, Message: "fail"}
}
func (p *streamFailProvider) Name() string { return p.name }

// streamOKProvider streams a single terminal chunk.
type streamOKProvider struct{ name string }

func (p *streamOKProvider) Chat(context.Context, *domain.OpenAIRequest, string) (*domain.OpenAIResponse, error) {
	return nil, nil
}
func (p *streamOKProvider) StreamChat(context.Context, *domain.OpenAIRequest, string) (<-chan domain.SSEChunk, error) {
	ch := make(chan domain.SSEChunk, 1)
	ch <- domain.SSEChunk{Done: true}
	close(ch)
	return ch, nil
}
func (p *streamOKProvider) Name() string { return p.name }

// TestGatewayService_StreamChatWithConnect_OnFallback verifies the onConnect callback
// fires after the connection/fallback decision succeeds but BEFORE any event is read,
// carrying the winning route and fallback count — so callers can set response headers
// (e.g. x-crosslink-fallback-*) that depend on the chosen route.
func TestGatewayService_StreamChatWithConnect_OnFallback(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register("A", &streamFailProvider{name: "A"})
	reg.Register("B", &streamOKProvider{name: "B"})

	// A is primary (weight 1), B is fallback (weight 0 → sorts last) for determinism.
	repo := &mockRepo{
		data: map[string][]model.ProviderModel{
			"claude-3": {
				{ID: 1, ProviderModel: "a-model", Weight: 1, Status: 1, Provider: model.Provider{Name: "A", Status: 1, ID: 1}},
				{ID: 2, ProviderModel: "b-model", Weight: 0, Status: 1, Provider: model.Provider{Name: "B", Status: 1, ID: 2}},
			},
		},
	}
	resolver := router.NewResolver(reg, repo, nil, map[router.StrategyName]router.RoutingStrategy{
		router.StrategyWeightedRandom: &router.WeightedRandomStrategy{},
	}, nil, nil, nil)
	svc := NewGatewayService(resolver, reg, nil, nil, nil)

	req := &domain.AnthropicRequest{Model: "claude-3", MaxTokens: 100, Messages: makeMessages("hi")}

	var connectModel string
	var connectFallback int
	var eventsAtConnect int32
	var eventCount int32

	_, err := svc.StreamChatWithConnect(context.Background(), req,
		func(_ context.Context, _ StreamEvent) bool {
			atomic.AddInt32(&eventCount, 1)
			return true
		},
		func(route *router.RouteResult, fallbackCount int) {
			connectModel = route.ProviderModel
			connectFallback = fallbackCount
			eventsAtConnect = atomic.LoadInt32(&eventCount)
		},
		"", 0)
	if err != nil {
		t.Fatalf("StreamChatWithConnect error: %v", err)
	}
	if connectModel != "b-model" {
		t.Fatalf("onConnect model = %q, want b-model (fallback target)", connectModel)
	}
	if connectFallback != 1 {
		t.Fatalf("onConnect fallbackCount = %d, want 1", connectFallback)
	}
	if eventsAtConnect != 0 {
		t.Fatalf("onConnect must fire before any event; saw %d events first", eventsAtConnect)
	}
}
