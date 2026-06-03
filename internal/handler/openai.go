package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	"github.com/crosslink/pkg/token"
	"gorm.io/datatypes"
)

type OpenAIHandler struct {
	resolver      *router.Resolver
	health        *provider.HealthTracker
	usageSvc      *service.UsageService
	latencySvc    *service.LatencyService
	activeTracker service.ProviderLoadTracker
	idemCache     *service.IdempotencyCache
	budget        *provider.RetryBudget
	guardrailSvc  *guardrail.GuardrailService
}

func NewOpenAIHandler(resolver *router.Resolver, usageSvc *service.UsageService, latencySvc *service.LatencyService, _ interface{}, activeTracker service.ProviderLoadTracker, idemCache *service.IdempotencyCache, budget *provider.RetryBudget, guardrailSvc *guardrail.GuardrailService) *OpenAIHandler {
	h := &OpenAIHandler{
		resolver:      resolver,
		usageSvc:      usageSvc,
		latencySvc:    latencySvc,
		activeTracker: activeTracker,
		idemCache:     idemCache,
		budget:        budget,
		guardrailSvc:  guardrailSvc,
	}
	if resolver != nil {
		h.health = resolver.Health()
	}
	return h
}

func (h *OpenAIHandler) logFailure(c *gin.Context, reqModel string, statusCode int, start time.Time, routes []*router.RouteResult, result *service.FallbackResult, retryCount int) {
	var keyID int64
	var teamID int64
	orgID := c.GetInt64("org_id")
	if key := middleware.GetAPIKeyFromContext(c); key != nil {
		keyID = key.ID
		if key.TeamID != nil {
			teamID = *key.TeamID
		}
	}
	var currency string
	var providerID int64
	if route := service.LastAttemptRoute(result, routes); route != nil {
		currency = route.Currency
		providerID = route.ProviderRow.ID
	}
	c.Set("usage_logged", true)
	submitUsage(func() {
		h.usageSvc.Log(context.Background(), &service.UsageEntry{
			RouteType:      "openai",
			ModelRequested: reqModel,
			ProviderID:     providerID,
			APIKeyID:       keyID,
			TeamID:         teamID,
			OrgID:          orgID,
			Currency:       currency,
			StatusCode:     statusCode,
			ErrorType:      "provider_error",
			LatencyMs:      time.Since(start).Milliseconds(),
			FallbackCount:  result.FallbackCount,
			RetryCount:     retryCount,
		})
	})
}

func extractLastOpenAIUserMessage(messages []domain.OpenAIMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return domain.ContentText(messages[i].Content)
		}
	}
	return ""
}

func mapProviderErrorStatus(err error) int {
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		switch pe.StatusCode {
		case http.StatusTooManyRequests, http.StatusBadRequest:
			return pe.StatusCode
		}
	}
	return http.StatusBadGateway
}

// safeProviderError returns a client-safe error message.
// It preserves provider error messages (already user-facing) but replaces
// internal errors with a generic message to avoid leaking infrastructure details.
func safeProviderError(err error) string {
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		return pe.Message
	}
	return "upstream provider error"
}

const maxRequestBody = 10 << 20 // 10 MB
const maxResponseBuffer = 1 << 20 // 1 MB buffer cap for content logging

func (h *OpenAIHandler) HandleChatCompletions(c *gin.Context) {
	var body []byte
	if cached := middleware.GetBodyBytes(c); cached != nil {
		body = cached
	} else {
		var err error
		body, err = io.ReadAll(io.LimitReader(c.Request.Body, maxRequestBody))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": map[string]string{"message": "failed to read body"}})
			return
		}
	}

	var req domain.OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": map[string]string{"message": "invalid json"}})
		return
	}

	start := time.Now()

	c.Set("model", req.Model)
	c.Set("stream", req.Stream)

	orgID := c.GetInt64("org_id")
	routes, err := h.resolver.Resolve(c.Request.Context(), req.Model, orgID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": map[string]string{"message": safeProviderError(err)}})
		return
	}

	sessionID := c.GetHeader("X-Session-ID")
	
	if req.Stream {
		h.handleStream(c, routes, &req, start, sessionID)
		return
	}

	h.handleNonStream(c, routes, &req, start, sessionID)
}

func (h *OpenAIHandler) handleNonStream(c *gin.Context, routes []*router.RouteResult, req *domain.OpenAIRequest, start time.Time, sessionID string) {
	orgID := c.GetInt64("org_id")
	// Idempotency cache check
	if idemKey := c.GetHeader("X-Idempotency-Key"); idemKey != "" && h.idemCache != nil {
		var idemKeyID int64
		if key := middleware.GetAPIKeyFromContext(c); key != nil {
			idemKeyID = key.ID
		}
		if cached, ok := h.idemCache.Get(c.Request.Context(), idemKeyID, idemKey); ok {
			safeHeaders := map[string]bool{"Content-Type": true, "X-Request-Id": true, "X-RateLimit-Limit": true, "X-RateLimit-Remaining": true, "X-RateLimit-Reset": true}
			for k, v := range cached.Headers {
				if safeHeaders[k] {
					c.Header(k, v)
				}
			}
			c.Data(cached.StatusCode, "application/json", cached.Body)
			return
		}
	}

	routes = service.ExpandFallbackRoutes(c.Request.Context(), h.resolver, routes, orgID)
	config := service.ResolveFallbackConfig(routes)
	engine := service.NewFallbackEngine(h.health, config)

	var totalRetries int
	result := engine.ExecuteNonStream(c.Request.Context(), routes, func(ctx context.Context, route *router.RouteResult) (any, error) {
		reqCopy := *req
		reqCopy.Model = route.ProviderModel
		pn := route.Provider.Name()
		if h.activeTracker != nil {
			h.activeTracker.Incr(ctx, pn)
		}
		retryCfg := route.RetryConfig
		if len(routes) > 1 {
			retryCfg.NumRetries = 0
		}
		var resp *domain.OpenAIResponse
		rr := provider.WithRetry(ctx, retryCfg, h.budget, func() error {
			var callErr error
			resp, callErr = route.Provider.Chat(ctx, &reqCopy, route.ProviderRow.APIKey)
			return callErr
		})
		totalRetries += rr.RetriesUsed
		if h.activeTracker != nil {
			h.activeTracker.Decr(context.Background(), pn)
		}
		if rr.Err != nil {
			return nil, rr.Err
		}
		return resp, nil
	})

	if result.FinalError != nil {
		slog.Error("all openai providers failed", "model", req.Model, "attempts", len(result.Attempts))
		statusCode := mapProviderErrorStatus(result.FinalError)
		h.logFailure(c, req.Model, statusCode, start, routes, result, totalRetries)
		c.JSON(statusCode, gin.H{"error": map[string]string{"message": safeProviderError(result.FinalError)}})
		return
	}

	resp, ok := result.Response.(*domain.OpenAIResponse)
	if !ok || resp == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": map[string]string{"message": "unexpected response type"}})
		return
	}
	route := result.Route

	respBody, err := json.Marshal(resp)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": map[string]string{"message": "failed to marshal response"}})
		return
	}

	// Extract identity for guardrail and usage logging
	var apiKeyID, teamID int64
	if key := middleware.GetAPIKeyFromContext(c); key != nil {
		apiKeyID = key.ID
		if key.TeamID != nil {
			teamID = *key.TeamID
		}
	}

	// Response-side guardrail check
	if h.guardrailSvc != nil && h.guardrailSvc.IsEnabled() {
		var respParts []string
		for _, ch := range resp.Choices {
			if text := domain.ContentText(ch.Message.Content); text != "" {
				respParts = append(respParts, text)
			}
		}
		respText := strings.Join(respParts, "\n")
		if respText != "" {
			grResult, grErr := h.guardrailSvc.Check(c.Request.Context(), &guardrail.CheckRequest{
				Content:   respText,
				Direction: guardrail.DirectionResponse,
				Model:     req.Model,
				APIKeyID:  apiKeyID,
				TeamID:    teamID,
				OrgID:     orgID,
			})
				if grErr != nil {
					if h.guardrailSvc.IsFailOpen() {
						slog.Warn("guardrail: response check failed, fail-open", "error", grErr)
					} else {
						slog.Error("guardrail: response check failed", "error", grErr)
						c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"type": "guardrail_error", "message": "guardrail service unavailable"}})
						return
					}
				} else if grResult != nil && grResult.Blocked {
					if grResult.Action == "block" {
						c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "guardrail_blocked", "message": "blocked by guardrail"}})
						return
					}
					if grResult.Action == "mask" && grResult.MaskedContent != "" {
						c.Set("guardrail_triggered", true)
						c.Set("guardrail_rule", grResult.RuleName)
						for i := range resp.Choices {
							resp.Choices[i].Message.Content = grResult.MaskedContent
						}
						respBody, _ = json.Marshal(resp)
					}
					if grResult.Action == "log" {
						c.Set("guardrail_triggered", true)
						c.Set("guardrail_rule", grResult.RuleName)
					}
				} else if grResult != nil && grResult.Action == "log" {
					c.Set("guardrail_triggered", true)
					c.Set("guardrail_rule", grResult.RuleName)
				}
			}
		}
	c.Header("Content-Type", "application/json")
	c.Status(http.StatusOK)
	c.Writer.Write(respBody)

	// Cache response for idempotency
	if idemKey := c.GetHeader("X-Idempotency-Key"); idemKey != "" && h.idemCache != nil {
		h.idemCache.Set(c.Request.Context(), apiKeyID, idemKey, &service.CachedResponse{
				StatusCode: http.StatusOK,
				Body:       respBody,
			})
	}

	inputTokens := resp.Usage.PromptTokens
	outputTokens := resp.Usage.CompletionTokens
	c.Set("input_tokens", inputTokens)
	c.Set("output_tokens", outputTokens)
	c.Set("input_price", route.InputPrice)
	c.Set("output_price", route.OutputPrice)

	latency := time.Since(start).Milliseconds()
	pn := route.Provider.Name()
	c.Set("provider", pn)
	c.Set("usage_logged", true)
	submitUsage(func() {
		if h.latencySvc != nil {
			h.latencySvc.RecordLatency(context.Background(), pn, latency)
		}
		entry := &service.UsageEntry{
			RouteType:      "openai",
			ModelRequested: req.Model,
			ModelUsed:      route.ProviderModel,
			ProviderID:     route.ProviderRow.ID,
			APIKeyID:       apiKeyID,
			TeamID:         teamID,
			OrgID:          orgID,
			InputTokens:    inputTokens,
			OutputTokens:   outputTokens,
			InputPrice:     route.InputPrice,
			Currency:       route.Currency,
			OutputPrice:    route.OutputPrice,
			StatusCode:     http.StatusOK,
			LatencyMs:      latency,
			FallbackCount:  result.FallbackCount,
			RetryCount:     totalRetries,
		}
		if v, ok := c.Get("guardrail_triggered"); ok {
			if b, _ := v.(bool); b {
				entry.GuardrailTriggered = true
			}
		}
		if v, ok := c.Get("guardrail_rule"); ok {
			if s, _ := v.(string); s != "" {
				entry.GuardrailRule = s
			}
		}
		if v, ok := c.Get("agent_type"); ok {
			if s, _ := v.(string); s != "" {
				entry.AgentType = s
			}
		}
		if v, ok := c.Get("security_events"); ok {
			if d, _ := v.(datatypes.JSON); d != nil {
				entry.SecurityEvents = d
			}
		}
		if v, ok := c.Get("cache_hit"); ok {
			if b, _ := v.(bool); b {
				entry.CacheHit = true
			}
		}
		if h.usageSvc.IsContentLogEnabled() {
			entry.UserMessage = truncateContent(extractLastOpenAIUserMessage(req.Messages))
			if len(resp.Choices) > 0 {
				entry.ModelResponse = truncateContent(domain.ContentText(resp.Choices[0].Message.Content))
			}
		}
		h.usageSvc.Log(context.Background(), entry)
	})
}

func (h *OpenAIHandler) handleStream(c *gin.Context, routes []*router.RouteResult, req *domain.OpenAIRequest, start time.Time, sessionID string) {
	orgID := c.GetInt64("org_id")
	routes = service.ExpandFallbackRoutes(c.Request.Context(), h.resolver, routes, orgID)
	config := service.ResolveFallbackConfig(routes)
	engine := service.NewFallbackEngine(h.health, config)

	var totalRetries int
	result := engine.ExecuteStream(c.Request.Context(), routes, func(ctx context.Context, route *router.RouteResult) (<-chan domain.SSEChunk, error) {
		reqCopy := *req
		reqCopy.Model = route.ProviderModel
		reqCopy.Stream = true
		if reqCopy.StreamOptions == nil {
			reqCopy.StreamOptions = &domain.StreamOptions{IncludeUsage: true}
		}
		pn := route.Provider.Name()
		if h.activeTracker != nil {
			h.activeTracker.Incr(ctx, pn)
		}
		retryCfg := route.RetryConfig
		if len(routes) > 1 {
			retryCfg.NumRetries = 0
		}
		var streamCh <-chan domain.SSEChunk
		rr := provider.WithRetry(ctx, retryCfg, h.budget, func() error {
			var callErr error
			streamCh, callErr = route.Provider.StreamChat(ctx, &reqCopy, route.ProviderRow.APIKey)
			return callErr
		})
		totalRetries += rr.RetriesUsed
		if h.activeTracker != nil {
			h.activeTracker.Decr(context.Background(), pn)
		}
		if rr.Err != nil {
			return nil, rr.Err
		}
		return streamCh, nil
	})

	if result.FinalError != nil {
		slog.Error("all stream providers failed", "model", req.Model, "attempts", len(result.Attempts))
		statusCode := mapProviderErrorStatus(result.FinalError)
		h.logFailure(c, req.Model, statusCode, start, routes, result, totalRetries)
		c.JSON(statusCode, gin.H{"error": map[string]string{"message": safeProviderError(result.FinalError)}})
		return
	}

	ch := result.StreamCh
	route := result.Route

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		slog.Error("response writer does not support flushing")
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "streaming not supported"}})
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	var inputTokens, outputTokens int
	var outputEstimate int
	var modelRespBuf strings.Builder
	var gotDone bool
	var firstTokenAt time.Time

	var apiKeyID, teamID int64
	if key := middleware.GetAPIKeyFromContext(c); key != nil {
		apiKeyID = key.ID
		if key.TeamID != nil {
			teamID = *key.TeamID
		}
	}

	if h.guardrailSvc != nil && h.guardrailSvc.IsEnabled() {
		wrapper := guardrail.NewStreamGuardrailWrapper(ch, h.guardrailSvc, req.Model, apiKeyID, teamID, orgID)
		for {
			sr := wrapper.Next(c.Request.Context())
			if sr.Done {
				break
			}
			if sr.Blocked != nil {
				c.Set("guardrail_triggered", true)
				c.Set("guardrail_rule", sr.Blocked.RuleName)
				if sr.Blocked.Action == "mask" && sr.Blocked.MaskedContent != "" {
					maskData, _ := json.Marshal(map[string]any{
						"choices": []map[string]any{{
							"delta": map[string]any{"content": sr.Blocked.MaskedContent},
						}},
					})
					if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", maskData); err != nil {
						return
					}
					flusher.Flush()
					continue
				}
				errData, _ := json.Marshal(map[string]any{
					"error": map[string]string{"type": "guardrail_blocked", "message": "blocked by guardrail"},
				})
				if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", errData); err != nil {
					return
				}
				fmt.Fprintln(c.Writer, "data: [DONE]")
				fmt.Fprintln(c.Writer)
				flusher.Flush()
				return
			}
			chunk := sr.Chunk
			if chunk.Done {
				gotDone = true
				fmt.Fprintln(c.Writer, "data: [DONE]")
				fmt.Fprintln(c.Writer)
				flusher.Flush()
				break
			}
			if chunk.Chunk == nil {
				continue
			}
			data, err := json.Marshal(chunk.Chunk)
			if err != nil {
				slog.Warn("marshal stream chunk failed", "error", err)
				continue
			}
			if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
			if chunk.Chunk.Usage != nil {
				inputTokens = chunk.Chunk.Usage.PromptTokens
				outputTokens = chunk.Chunk.Usage.CompletionTokens
			}
			if len(chunk.Chunk.Choices) > 0 {
				content := chunk.Chunk.Choices[0].Delta.Content
				if h.usageSvc.IsContentLogEnabled() && content != "" && modelRespBuf.Len() < maxResponseBuffer {
					modelRespBuf.WriteString(content)
				}
				if content != "" {
					outputEstimate += token.Estimate(content)
					if firstTokenAt.IsZero() {
						firstTokenAt = time.Now()
					}
				}
			}
		}
	} else {
		streamCtx := c.Request.Context()
		for chunk := range ch {
			select {
			case <-streamCtx.Done():
				// Client disconnected — drain remaining chunks
				go func() {
					drainCtx, drainCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer drainCancel()
					for {
						select {
						case _, ok := <-ch:
							if !ok {
								return
							}
						case <-drainCtx.Done():
							return
						}
					}
				}()
				return
			default:
			}
			if chunk.Done {
				gotDone = true
				fmt.Fprintln(c.Writer, "data: [DONE]")
				fmt.Fprintln(c.Writer)
				flusher.Flush()
				break
			}

			if chunk.Chunk == nil {
				continue
			}

			data, err := json.Marshal(chunk.Chunk)
			if err != nil {
				slog.Warn("marshal stream chunk failed", "error", err)
				continue
			}
			if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()

			if chunk.Chunk.Usage != nil {
				inputTokens = chunk.Chunk.Usage.PromptTokens
				outputTokens = chunk.Chunk.Usage.CompletionTokens
			}
			if len(chunk.Chunk.Choices) > 0 {
				content := chunk.Chunk.Choices[0].Delta.Content
				if h.usageSvc.IsContentLogEnabled() && content != "" && modelRespBuf.Len() < maxResponseBuffer {
					modelRespBuf.WriteString(content)
				}
				if content != "" {
					outputEstimate += token.Estimate(content)
					if firstTokenAt.IsZero() {
						firstTokenAt = time.Now()
					}
				}
			}
		}
	}

	// Graceful degradation: if provider disconnected mid-stream, send [DONE]
	if !gotDone {
		slog.Warn("stream disconnected mid-stream, sending graceful end",
			"provider", route.Provider.Name(),
			"output_tokens", outputTokens)
		fmt.Fprintln(c.Writer, "data: [DONE]")
		fmt.Fprintln(c.Writer)
		flusher.Flush()
	}

	// Fallback to estimation if provider didn't return usage
	if outputTokens == 0 && outputEstimate > 0 {
		outputTokens = outputEstimate
	}
	// Fallback input token estimation when provider didn't send usage in stream
	if inputTokens == 0 && req.Messages != nil {
		for _, msg := range req.Messages {
			inputTokens += token.Estimate(domain.ContentText(msg.Content))
			for _, tc := range msg.ToolCalls {
				inputTokens += token.Estimate(tc.Function.Arguments)
			}
		}
	}

	c.Set("input_tokens", inputTokens)
	c.Set("output_tokens", outputTokens)
	c.Set("input_price", route.InputPrice)
	c.Set("output_price", route.OutputPrice)

	latency := time.Since(start).Milliseconds()
	pn := route.Provider.Name()
	c.Set("provider", pn)
	c.Set("usage_logged", true)
	submitUsage(func() {
		if h.latencySvc != nil {
			h.latencySvc.RecordLatency(context.Background(), pn, latency)
		}
		entry := &service.UsageEntry{
			RouteType:      "openai",
			ModelRequested: req.Model,
			ModelUsed:      route.ProviderModel,
			ProviderID:     route.ProviderRow.ID,
			APIKeyID:       apiKeyID,
			TeamID:         teamID,
			OrgID:          orgID,
			InputTokens:    inputTokens,
			OutputTokens:   outputTokens,
			InputPrice:     route.InputPrice,
			Currency:       route.Currency,
			OutputPrice:    route.OutputPrice,
			StatusCode:     http.StatusOK,
			LatencyMs:      latency,
			FirstTokenMs:   firstTokenMsValue(start, firstTokenAt),
			FallbackCount:  result.FallbackCount,
			RetryCount:     totalRetries,
		}
		if v, ok := c.Get("guardrail_triggered"); ok {
			if b, _ := v.(bool); b {
				entry.GuardrailTriggered = true
			}
		}
		if v, ok := c.Get("guardrail_rule"); ok {
			if s, _ := v.(string); s != "" {
				entry.GuardrailRule = s
			}
		}
		if v, ok := c.Get("agent_type"); ok {
			if s, _ := v.(string); s != "" {
				entry.AgentType = s
			}
		}
		if v, ok := c.Get("security_events"); ok {
			if d, _ := v.(datatypes.JSON); d != nil {
				entry.SecurityEvents = d
			}
		}
		if v, ok := c.Get("cache_hit"); ok {
			if b, _ := v.(bool); b {
				entry.CacheHit = true
			}
		}
		if h.usageSvc.IsContentLogEnabled() {
			entry.UserMessage = truncateContent(extractLastOpenAIUserMessage(req.Messages))
			entry.ModelResponse = truncateContent(modelRespBuf.String())
		}
		h.usageSvc.Log(context.Background(), entry)
	})
}
func firstTokenMsValue(start time.Time, firstTokenAt time.Time) int64 {
	if firstTokenAt.IsZero() {
		return 0
	}
	return firstTokenAt.Sub(start).Milliseconds()
}

