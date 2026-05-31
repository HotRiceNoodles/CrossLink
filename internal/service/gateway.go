package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/translator"
	"github.com/crosslink/pkg/token"
)

type GatewayService struct {
	resolver      *router.Resolver
	health        *provider.HealthTracker
	latencySvc    *LatencyService
	activeTracker ProviderLoadTracker
	budget        *provider.RetryBudget
}

func NewGatewayService(resolver *router.Resolver, _ *provider.Registry, latencySvc *LatencyService, activeTracker ProviderLoadTracker, budget *provider.RetryBudget) *GatewayService {
	svc := &GatewayService{
		resolver:      resolver,
		latencySvc:    latencySvc,
		activeTracker: activeTracker,
		budget:        budget,
	}
	if resolver != nil {
		svc.health = resolver.Health()
	}
	return svc
}

type ChatResult struct {
	Response      *domain.AnthropicResponse
	InputTokens   int
	OutputTokens  int
	LatencyMs     int64
	ProviderName  string
	ProviderID    int64
	ModelUsed     string
	InputPrice    float64
	OutputPrice   float64
	Currency      string
	FallbackCount int
	RetryCount    int
}

func (s *GatewayService) Chat(ctx context.Context, req *domain.AnthropicRequest, sessionID string, orgID int64) (*ChatResult, error) {
	start := time.Now()

	routes, err := s.resolver.Resolve(ctx, req.Model, orgID)
	if err != nil {
		return nil, fmt.Errorf("resolve route: %w", err)
	}

	routes = ExpandFallbackRoutes(ctx, s.resolver, routes, orgID)

	openaiReq, err := translator.AnthropicToOpenAI(req, "")
	if err != nil {
		return nil, fmt.Errorf("translate request: %w", err)
	}

	config := ResolveFallbackConfig(routes)
	engine := NewFallbackEngine(s.health, config)

	var totalRetries int
	result := engine.ExecuteNonStream(ctx, routes, func(ctx context.Context, route *router.RouteResult) (any, error) {
		reqCopy := *openaiReq
		reqCopy.Model = route.ProviderModel
		pn := route.Provider.Name()
		if s.activeTracker != nil {
			s.activeTracker.Incr(ctx, pn)
		}
		retryCfg := route.RetryConfig
		if len(routes) > 1 {
			retryCfg.NumRetries = 0
		}
		var openaiResp *domain.OpenAIResponse
		rr := provider.WithRetry(ctx, retryCfg, s.budget, func() error {
			var callErr error
			openaiResp, callErr = route.Provider.Chat(ctx, &reqCopy, route.ProviderRow.APIKey)
			return callErr
		})
		totalRetries += rr.RetriesUsed
		if s.activeTracker != nil {
			s.activeTracker.Decr(ctx, pn)
		}
		if rr.Err != nil {
			return nil, rr.Err
		}
		return openaiResp, nil
	})

	if result.FinalError != nil {
		route := LastAttemptRoute(result, routes)
		if route != nil {
			return nil, &RouteError{Route: route, Inner: result.FinalError, Model: req.Model, FallbackCount: result.FallbackCount, RetryCount: totalRetries}
		}
		return nil, fmt.Errorf("all providers failed for model %s: %w", req.Model, result.FinalError)
	}

	openaiResp, ok := result.Response.(*domain.OpenAIResponse)
	if !ok || openaiResp == nil {
		return nil, fmt.Errorf("unexpected response type for model %s", req.Model)
	}
	route := result.Route

	anthropicResp, err := translator.OpenAIToAnthropic(openaiResp, req.Model)
	if err != nil {
		return nil, fmt.Errorf("translate response: %w", err)
	}

	latency := time.Since(start).Milliseconds()
	providerName := route.Provider.Name()
	inputTokens := openaiResp.Usage.PromptTokens
	outputTokens := openaiResp.Usage.CompletionTokens
	slog.Info("request completed",
		"model", req.Model,
		"provider", providerName,
		"provider_model", route.ProviderModel,
		"latency_ms", latency,
		"input_tokens", inputTokens,
		"output_tokens", outputTokens,
	)

	if s.latencySvc != nil {
		s.latencySvc.RecordLatency(ctx, providerName, latency)
	}
	return &ChatResult{
		Response:      anthropicResp,
		InputTokens:   openaiResp.Usage.PromptTokens,
		OutputTokens:  openaiResp.Usage.CompletionTokens,
		LatencyMs:     latency,
		ProviderName:  providerName,
		ProviderID:    route.ProviderRow.ID,
		ModelUsed:     route.ProviderModel,
		InputPrice:    route.InputPrice,
		OutputPrice:   route.OutputPrice,
		Currency:      route.Currency,
		FallbackCount: result.FallbackCount,
		RetryCount:    totalRetries,
	}, nil
}

type StreamEvent struct {
	Event string
	Data  string
}

type StreamChatFunc func(ctx context.Context, event StreamEvent) bool

func (s *GatewayService) StreamChat(ctx context.Context, req *domain.AnthropicRequest, fn StreamChatFunc, sessionID string, orgID int64) (*StreamResult, error) {
	start := time.Now()

	routes, err := s.resolver.Resolve(ctx, req.Model, orgID)
	if err != nil {
		return nil, fmt.Errorf("resolve route: %w", err)
	}

	routes = ExpandFallbackRoutes(ctx, s.resolver, routes, orgID)

	openaiReq, err := translator.AnthropicToOpenAI(req, "")
	if err != nil {
		return nil, fmt.Errorf("translate request: %w", err)
	}
	openaiReq.Stream = true
	openaiReq.StreamOptions = &domain.StreamOptions{IncludeUsage: true}

	config := ResolveFallbackConfig(routes)
	engine := NewFallbackEngine(s.health, config)
	var totalRetries int

	result := engine.ExecuteStream(ctx, routes, func(ctx context.Context, route *router.RouteResult) (<-chan domain.SSEChunk, error) {
		reqCopy := *openaiReq
		reqCopy.Model = route.ProviderModel
		pn := route.Provider.Name()
		if s.activeTracker != nil {
			s.activeTracker.Incr(ctx, pn)
		}
		retryCfg := route.RetryConfig
		if len(routes) > 1 {
			retryCfg.NumRetries = 0
		}
		var ch <-chan domain.SSEChunk
		rr := provider.WithRetry(ctx, retryCfg, s.budget, func() error {
			var callErr error
			ch, callErr = route.Provider.StreamChat(ctx, &reqCopy, route.ProviderRow.APIKey)
			return callErr
		})
		totalRetries += rr.RetriesUsed
		if s.activeTracker != nil {
			s.activeTracker.Decr(ctx, pn)
		}
		if rr.Err != nil {
			return nil, rr.Err
		}
		return ch, nil
	})

	if result.FinalError != nil {
		route := LastAttemptRoute(result, routes)
		if route != nil {
			return nil, &RouteError{Route: route, Inner: result.FinalError, Model: req.Model, FallbackCount: result.FallbackCount, RetryCount: totalRetries}
		}
		return nil, fmt.Errorf("all providers failed for model %s: %w", req.Model, result.FinalError)
	}

	ch := result.StreamCh
	route := result.Route

	messageID := translator.GenerateMessageID()
	inputEstimate := estimateInputTokens(openaiReq)
	st := translator.NewStreamTranslator(messageID, req.Model, inputEstimate)

	stopped := false
loop:
	for sseChunk := range ch {
		events := st.TranslateChunk(sseChunk)
		for _, event := range events {
			if !fn(ctx, StreamEvent{Event: event.Event, Data: string(event.Data)}) {
				stopped = true
				break loop
			}
		}
	}

	if !stopped {
		disconnectEvents := st.OnProviderDisconnect()
		for _, event := range disconnectEvents {
			fn(ctx, StreamEvent{Event: event.Event, Data: string(event.Data)})
		}
	}

	latency := time.Since(start).Milliseconds()
	providerName := route.Provider.Name()
	slog.Info("stream completed",
		"model", req.Model,
		"provider", providerName,
		"provider_model", route.ProviderModel,
		"latency_ms", latency,
		"input_tokens", st.InputTokens(),
		"output_tokens", st.OutputTokens(),
	)

	if s.latencySvc != nil {
		s.latencySvc.RecordLatency(ctx, providerName, latency)
	}
	return &StreamResult{
		InputTokens:   st.InputTokens(),
		OutputTokens:  st.OutputTokens(),
		LatencyMs:     latency,
		ProviderName:  providerName,
		ProviderID:    route.ProviderRow.ID,
		ModelUsed:     route.ProviderModel,
		InputPrice:    route.InputPrice,
		OutputPrice:   route.OutputPrice,
		Currency:      route.Currency,
		FallbackCount: result.FallbackCount,
		RetryCount:    totalRetries,
	}, nil
}

// estimateInputTokens returns a rough token count from the OpenAI request messages.
func estimateInputTokens(req *domain.OpenAIRequest) int {
	n := 0
	for _, msg := range req.Messages {
		n += token.Estimate(domain.ContentText(msg.Content))
		for _, tc := range msg.ToolCalls {
			n += token.Estimate(tc.Function.Arguments)
		}
		if msg.Role == "tool" && msg.ToolCallID != "" {
			if s, ok := msg.Content.(string); ok {
				n += token.Estimate(s)
			}
		}
	}
	return n
}

type StreamResult struct {
	InputTokens   int
	OutputTokens  int
	LatencyMs     int64
	ProviderName  string
	ProviderID    int64
	ModelUsed     string
	InputPrice    float64
	OutputPrice   float64
	Currency      string
	FallbackCount int
	RetryCount    int
}

type RouteError struct {
	Route         *router.RouteResult
	Inner         error
	Model         string
	FallbackCount int
	RetryCount    int
}

func (e *RouteError) Error() string {
	return fmt.Sprintf("all providers failed for model %s: %v", e.Model, e.Inner)
}

func (e *RouteError) Unwrap() error {
	return e.Inner
}
