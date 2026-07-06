package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/crosslink/internal/debug/upstream"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/translator"
	"github.com/crosslink/pkg/token"
	"github.com/redis/go-redis/v9"
)

type GatewayService struct {
	resolver      *router.Resolver
	health        *provider.HealthTracker
	latencySvc    *LatencyService
	activeTracker ProviderLoadTracker
	budget        *provider.RetryBudget
	classifier    *ErrorClassifier
	guardRDB      *redis.Client // P3a per-(provider,model) guardrails; nil = disabled
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

// SetClassifier injects the error classifier used by fallback engines created by this
// service (NB1 injection chain).
func (s *GatewayService) SetClassifier(c *ErrorClassifier) { s.classifier = c }

// SetGuardRDB injects the Redis client used for P3a per-(provider,model)
// guardrail counters (covers the Anthropic dispatch path). nil disables.
func (s *GatewayService) SetGuardRDB(rdb *redis.Client) { s.guardRDB = rdb }

type ChatResult struct {
	Response        *domain.AnthropicResponse
	InputTokens     int
	OutputTokens    int
	LatencyMs       int64
	ProviderName    string
	ProviderID      int64
	ModelUsed       string
	InputPrice      float64
	OutputPrice     float64
	Currency        string
	FallbackCount   int
	RetryCount      int
	ReasoningTokens int
	CacheReadTokens int
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
	engine.SetClassifier(s.classifier)

	var totalRetries int
	result := engine.ExecuteNonStream(ctx, routes, func(ctx context.Context, route *router.RouteResult) (any, error) {
		reqCopy := *openaiReq
		reqCopy.Model = route.ProviderModel
		pn := route.Provider.Name()
		// P3a: per-(provider,model) guardrail counters (count-only; P4 enforces).
		if s.guardRDB != nil {
			conc, rpm := router.ParseGuardrailConfig(route.ExtraConfig)
			if conc > 0 || rpm > 0 {
				release := AcquireDispatchGuard(ctx, s.guardRDB, pn, route.ProviderModel, GuardrailConfig{Concurrency: conc, RPM: rpm})
				defer release()
			}
		}
		if s.activeTracker != nil {
			s.activeTracker.Incr(ctx, pn)
		}
		retryCfg := route.RetryConfig
		if len(routes) > 1 {
			retryCfg.NumRetries = 0
		}
		var openaiResp *domain.OpenAIResponse
		rr := provider.WithRetry(ctx, retryCfg, s.budget, func(attemptCtx context.Context) error {
			attemptCtx = upstream.WithProviderName(attemptCtx, route.Provider.Name())
			attemptCtx = upstream.WithProviderModel(attemptCtx, route.ProviderModel)
			attemptCtx = upstream.WithProviderBaseURL(attemptCtx, route.ProviderRow.BaseURL)
			var callErr error
			openaiResp, callErr = route.Provider.Chat(attemptCtx, &reqCopy, route.ProviderRow.APIKey)
			return callErr
		})
		totalRetries += rr.RetriesUsed
		if s.activeTracker != nil {
			s.activeTracker.Decr(ctx, pn)
		}
		if s.guardRDB != nil {
			RecordDispatchOutcome(context.Background(), s.guardRDB, pn, route.ProviderModel, rr.Err)
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
		Response:        anthropicResp,
		InputTokens:     openaiResp.Usage.PromptTokens,
		OutputTokens:    openaiResp.Usage.CompletionTokens,
		LatencyMs:       latency,
		ProviderName:    providerName,
		ProviderID:      route.ProviderRow.ID,
		ModelUsed:       route.ProviderModel,
		InputPrice:      route.InputPrice,
		OutputPrice:     route.OutputPrice,
		Currency:        route.Currency,
		FallbackCount:   result.FallbackCount,
		RetryCount:      totalRetries,
		ReasoningTokens: extractReasoningTokens(openaiResp.Usage.CompletionTokensDetails),
		CacheReadTokens: extractCacheReadTokens(openaiResp.Usage.PromptTokensDetails),
	}, nil
}

type StreamEvent struct {
	Event string
	Data  string
}

type StreamChatFunc func(ctx context.Context, event StreamEvent) bool

// StreamConnectFunc is invoked by StreamChatWithConnect after the connection/fallback
// decision succeeds but before the first event is read. It receives the winning route
// and fallback count so callers can set response headers that depend on the chosen route
// (e.g. x-crosslink-fallback-*), which must precede the streamed body.
type StreamConnectFunc func(route *router.RouteResult, fallbackCount int)

// StreamChat streams a chat completion, invoking fn for each translated event.
// Backward-compatible entry point (onConnect is nil). New callers that need to set
// route-dependent response headers should use StreamChatWithConnect.
func (s *GatewayService) StreamChat(ctx context.Context, req *domain.AnthropicRequest, fn StreamChatFunc, sessionID string, orgID int64) (*StreamResult, error) {
	return s.StreamChatWithConnect(ctx, req, fn, nil, sessionID, orgID)
}

// StreamChatWithConnect is StreamChat with an onConnect callback fired once the
// connection (and any fallback) has settled, before streaming begins.
func (s *GatewayService) StreamChatWithConnect(ctx context.Context, req *domain.AnthropicRequest, fn StreamChatFunc, onConnect StreamConnectFunc, sessionID string, orgID int64) (*StreamResult, error) {
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
	engine.SetClassifier(s.classifier)
	var totalRetries int

	result := engine.ExecuteStream(ctx, routes, func(ctx context.Context, route *router.RouteResult) (<-chan domain.SSEChunk, error) {
		reqCopy := *openaiReq
		reqCopy.Model = route.ProviderModel
		pn := route.Provider.Name()
		// P3a: per-(provider,model) guardrail counters (count-only; P4 enforces).
		if s.guardRDB != nil {
			conc, rpm := router.ParseGuardrailConfig(route.ExtraConfig)
			if conc > 0 || rpm > 0 {
				release := AcquireDispatchGuard(ctx, s.guardRDB, pn, route.ProviderModel, GuardrailConfig{Concurrency: conc, RPM: rpm})
				defer release()
			}
		}
		if s.activeTracker != nil {
			s.activeTracker.Incr(ctx, pn)
		}
		retryCfg := route.RetryConfig
		if len(routes) > 1 {
			retryCfg.NumRetries = 0
		}
		var ch <-chan domain.SSEChunk
		rr := provider.WithRetry(ctx, retryCfg, s.budget, func(attemptCtx context.Context) error {
			attemptCtx = upstream.WithProviderName(attemptCtx, route.Provider.Name())
			attemptCtx = upstream.WithProviderModel(attemptCtx, route.ProviderModel)
			attemptCtx = upstream.WithProviderBaseURL(attemptCtx, route.ProviderRow.BaseURL)
			var callErr error
			ch, callErr = route.Provider.StreamChat(attemptCtx, &reqCopy, route.ProviderRow.APIKey)
			return callErr
		})
		totalRetries += rr.RetriesUsed
		if s.activeTracker != nil {
			s.activeTracker.Decr(ctx, pn)
		}
		if s.guardRDB != nil {
			RecordDispatchOutcome(context.Background(), s.guardRDB, pn, route.ProviderModel, rr.Err)
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

	// Surface the winning route + fallback count before any event is read, so callers
	// can set response headers that depend on the chosen route.
	if onConnect != nil {
		onConnect(route, result.FallbackCount)
	}

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
		InputTokens:     st.InputTokens(),
		OutputTokens:    st.OutputTokens(),
		LatencyMs:       latency,
		ProviderName:    providerName,
		ProviderID:      route.ProviderRow.ID,
		ModelUsed:       route.ProviderModel,
		InputPrice:      route.InputPrice,
		OutputPrice:     route.OutputPrice,
		Currency:        route.Currency,
		FallbackCount:   result.FallbackCount,
		RetryCount:      totalRetries,
		ReasoningTokens: st.ReasoningTokens(),
		CacheReadTokens: st.CacheReadTokens(),
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
	InputTokens     int
	OutputTokens    int
	LatencyMs       int64
	ProviderName    string
	ProviderID      int64
	ModelUsed       string
	InputPrice      float64
	OutputPrice     float64
	Currency        string
	FallbackCount   int
	RetryCount      int
	ReasoningTokens int
	CacheReadTokens int
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

func extractReasoningTokens(d *domain.CompletionTokensDetails) int {
	if d != nil {
		return d.ReasoningTokens
	}
	return 0
}

func extractCacheReadTokens(d *domain.PromptTokensDetails) int {
	if d != nil {
		return d.CachedTokens
	}
	return 0
}
