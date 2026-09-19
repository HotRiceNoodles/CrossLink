package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/debug/upstream"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	"github.com/crosslink/pkg/token"
)

const maxRequestBody = 10 << 20 // 10 MB
const maxTTSAudioSize = 25 << 20 // 25 MB — playground TTS buffers audio in memory

type PlaygroundHandler struct {
	resolver      *router.Resolver
	health        *provider.HealthTracker
	usageSvc      *service.UsageService
	latencySvc    *service.LatencyService
	activeTracker service.ProviderLoadTracker
	budget        *provider.RetryBudget
	guardrailSvc  *guardrail.GuardrailService
	taskSvc       *service.VideoTaskService
}

func NewPlaygroundHandler(
	resolver *router.Resolver,
	usageSvc *service.UsageService,
	latencySvc *service.LatencyService,
	activeTracker service.ProviderLoadTracker,
	budget *provider.RetryBudget,
	guardrailSvc *guardrail.GuardrailService,
	taskSvc *service.VideoTaskService,
) *PlaygroundHandler {
	h := &PlaygroundHandler{
		resolver:      resolver,
		health:        resolver.Health(),
		usageSvc:      usageSvc,
		latencySvc:    latencySvc,
		activeTracker: activeTracker,
		budget:        budget,
		guardrailSvc:  guardrailSvc,
		taskSvc:       taskSvc,
	}
	return h
}

type PlaygroundRequest struct {
	Model     string                 `json:"model"`
	Messages  []domain.OpenAIMessage `json:"messages"`
	MaxTokens *int                   `json:"max_tokens,omitempty"`
	Temperature *float64               `json:"temperature,omitempty"`
	TopP        *float64               `json:"top_p,omitempty"`
	Stream      bool                   `json:"stream,omitempty"`
}

func bindPlaygroundRequest(c *gin.Context) (*PlaygroundRequest, bool) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxRequestBody))
	if err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "failed to read body")
		return nil, false
	}
	var req PlaygroundRequest
	if err := json.Unmarshal(body, &req); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "invalid request")
		return nil, false
	}
	if req.Model == "" || len(req.Messages) == 0 {
		errorResp(c, http.StatusBadRequest, ErrModelRequired, "model and messages are required")
		return nil, false
	}
	if len(req.Messages) > 100 {
		errorResp(c, http.StatusBadRequest, ErrTooManyMessages, "too many messages (max 100)")
		return nil, false
	}
	return &req, true
}

func mapPlaygroundErrorStatus(err error) int {
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		switch pe.StatusCode {
		case http.StatusTooManyRequests, http.StatusBadRequest:
			return pe.StatusCode
		}
	}
	return http.StatusBadGateway
}

func safeProviderError(err error) string {
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		return pe.Message
	}
	return "upstream provider error"
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (h *PlaygroundHandler) Chat(c *gin.Context) {
	req, ok := bindPlaygroundRequest(c)
	if !ok {
		return
	}

	orgID := c.GetInt64("org_id")
	routes, err := h.resolver.Resolve(c.Request.Context(), req.Model, orgID)
	if err != nil || len(routes) == 0 {
		slog.Error("playground: no route resolved", "model", req.Model, "error", err)
		errorResp(c, http.StatusNotFound, ErrNoRouteForModel, fmt.Sprintf("no available route for model: %s", req.Model))
		return
	}

	c.Set("model", req.Model)
	c.Set("stream", false)

	routes = service.ExpandFallbackRoutes(c.Request.Context(), h.resolver, routes, orgID)
	config := service.ResolveFallbackConfig(routes)
	engine := service.NewFallbackEngine(h.health, config)

	openaiReq := &domain.OpenAIRequest{
		Model:       req.Model,
		Messages:    req.Messages,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      false,
	}

	start := time.Now()
	var totalRetries int
	result := engine.ExecuteNonStream(c.Request.Context(), routes,
		func(ctx context.Context, route *router.RouteResult) (any, error) {
			reqCopy := *openaiReq
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
			rr := provider.WithRetry(ctx, retryCfg, h.budget, func(attemptCtx context.Context) error {
				attemptCtx = upstream.WithProviderName(attemptCtx, route.Provider.Name())
				attemptCtx = upstream.WithProviderModel(attemptCtx, route.ProviderModel)
				attemptCtx = upstream.WithProviderBaseURL(attemptCtx, route.ProviderRow.BaseURL)
				var callErr error
				resp, callErr = route.Provider.Chat(attemptCtx, &reqCopy, route.ProviderRow.APIKey)
				return callErr
			})
			totalRetries += rr.RetriesUsed
			if h.activeTracker != nil {
				h.activeTracker.Decr(ctx, pn)
			}
			if rr.Err != nil {
				return nil, rr.Err
			}
			return resp, nil
		},
	)

	if result.FinalError != nil {
		slog.Error("playground: chat request failed", "model", req.Model, "error", result.FinalError)
		c.JSON(mapPlaygroundErrorStatus(result.FinalError), gin.H{"error": safeProviderError(result.FinalError)})
		return
	}

	resp, ok := result.Response.(*domain.OpenAIResponse)
	if !ok || resp == nil {
		slog.Error("playground: unexpected response type", "model", req.Model)
		errorResp(c, http.StatusBadGateway, ErrUnexpectedResponse, "unexpected response")
		return
	}

	latencyMs := time.Since(start).Milliseconds()
	pn := result.Route.ProviderRow.Name

	// Response-side guardrail check
	if h.guardrailSvc != nil && h.guardrailSvc.IsEnabled() {
		respText := ""
		if len(resp.Choices) > 0 {
			respText = domain.ContentText(resp.Choices[0].Message.Content)
			}

		if respText != "" {
			grResult, grErr := h.guardrailSvc.Check(c.Request.Context(), &guardrail.CheckRequest{
				Content:   respText,
				Direction: guardrail.DirectionResponse,
				Model:     req.Model,
			APIKeyID:  0,
			TeamID:    0,
			OrgID:     0,
			})
			if grErr != nil {
				slog.Warn("playground: guardrail response check failed", "error", grErr)
			} else if grResult != nil && grResult.Blocked {
				if grResult.Action == "block" {
					// Upstream tokens were already consumed — record usage before returning.
					go h.logUsage(c.GetHeader("X-Session-ID"), orgID, openaiReq,
						resp.Usage.PromptTokens, resp.Usage.CompletionTokens,
						result.Route, pn, latencyMs, result.FallbackCount, totalRetries,
						true, grResult.RuleName, "")
					reason := "blocked by guardrail"
					if grResult.Reason != "" {
						reason = grResult.Reason
					}
					c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "guardrail_blocked", "message": reason}})
					return
				}
				if grResult.Action == "mask" && grResult.MaskedContent != "" {
					c.Set("guardrail_triggered", true)
					c.Set("guardrail_rule", grResult.RuleName)
					resp.Choices[0].Message.Content = grResult.MaskedContent
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

	c.Set("provider", pn)
	c.Set("input_tokens", resp.Usage.PromptTokens)
	c.Set("output_tokens", resp.Usage.CompletionTokens)
	c.Set("input_price", result.Route.InputPrice)
	c.Set("output_price", result.Route.OutputPrice)

	var respText string
	if len(resp.Choices) > 0 {
		respText = domain.ContentText(resp.Choices[0].Message.Content)
	}

	sessionID := c.GetHeader("X-Session-ID")
	grTriggered, grRule := guardrailContext(c)
	go h.logUsage(sessionID, orgID, openaiReq, resp.Usage.PromptTokens, resp.Usage.CompletionTokens,
		result.Route, pn, latencyMs, result.FallbackCount, totalRetries, grTriggered, grRule, respText)

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"response":   resp,
			"model":      result.Route.ProviderModel,
			"provider":   pn,
			"tokens":     gin.H{"input": resp.Usage.PromptTokens, "output": resp.Usage.CompletionTokens},
			"latency_ms": latencyMs,
		},
	})
}

func (h *PlaygroundHandler) StreamChat(c *gin.Context) {
	req, ok := bindPlaygroundRequest(c)
	if !ok {
		return
	}

	orgID := c.GetInt64("org_id")
	routes, err := h.resolver.Resolve(c.Request.Context(), req.Model, orgID)
	if err != nil || len(routes) == 0 {
		slog.Error("playground: no route resolved for stream", "model", req.Model, "error", err)
		errorResp(c, http.StatusNotFound, ErrNoRouteForModel, fmt.Sprintf("no available route for model: %s", req.Model))
		return
	}

	c.Set("model", req.Model)
	c.Set("stream", true)

	routes = service.ExpandFallbackRoutes(c.Request.Context(), h.resolver, routes, orgID)
	config := service.ResolveFallbackConfig(routes)
	engine := service.NewFallbackEngine(h.health, config)

	openaiReq := &domain.OpenAIRequest{
		Model:       req.Model,
		Messages:    req.Messages,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      true,
	}

	start := time.Now()
	var totalRetries int
	result := engine.ExecuteStream(c.Request.Context(), routes,
		func(ctx context.Context, route *router.RouteResult) (<-chan domain.SSEChunk, error) {
			reqCopy := *openaiReq
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
			var ch <-chan domain.SSEChunk
			rr := provider.WithRetry(ctx, retryCfg, h.budget, func(attemptCtx context.Context) error {
				attemptCtx = upstream.WithProviderName(attemptCtx, route.Provider.Name())
				attemptCtx = upstream.WithProviderModel(attemptCtx, route.ProviderModel)
				attemptCtx = upstream.WithProviderBaseURL(attemptCtx, route.ProviderRow.BaseURL)
				var callErr error
				ch, callErr = route.Provider.StreamChat(attemptCtx, &reqCopy, route.ProviderRow.APIKey)
				return callErr
			})
			totalRetries += rr.RetriesUsed
			if h.activeTracker != nil {
				h.activeTracker.Decr(ctx, pn)
			}
			if rr.Err != nil {
				return nil, rr.Err
			}
			return ch, nil
		},
	)

	if result.FinalError != nil {
		slog.Error("playground: stream request failed", "model", req.Model, "error", result.FinalError)
		c.JSON(mapPlaygroundErrorStatus(result.FinalError), gin.H{"error": safeProviderError(result.FinalError)})
		return
	}

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		slog.Error("playground: response writer does not support flushing")
		errorResp(c, http.StatusInternalServerError, ErrStreamingNotSupported, "streaming not supported")
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	flusher.Flush()

	pn := result.Route.ProviderRow.Name
	c.Set("provider", pn)
	c.Set("input_price", result.Route.InputPrice)
	c.Set("output_price", result.Route.OutputPrice)

	var inputTokens, outputTokens int
	var outputEstimate int
	var modelRespBuf strings.Builder
	var gotDone bool
	ctxDone := c.Request.Context().Done()

	if h.guardrailSvc != nil && h.guardrailSvc.IsEnabled() {
		wrapper := guardrail.NewStreamGuardrailWrapper(result.StreamCh, h.guardrailSvc, req.Model, 0, 0, 0)
	guardLoop:
		for {
			select {
			case <-ctxDone:
				slog.Info("playground: client disconnected during stream", "model", req.Model)
				break guardLoop
			default:
			}
			sr := wrapper.Next(c.Request.Context())
			if sr.Done {
				break
			}
			if sr.Blocked != nil {
				c.Set("guardrail_triggered", true)
				c.Set("guardrail_rule", sr.Blocked.RuleName)
				if sr.Blocked.Action == "mask" && sr.Blocked.MaskedContent != "" {
					maskData, _ := json.Marshal(map[string]any{
						"type":    "guardrail_masked",
						"content": sr.Blocked.MaskedContent,
					})
					fmt.Fprintf(c.Writer, "data: %s\n\n", maskData)
					flusher.Flush()
					continue
				}
				reason := "blocked by guardrail"
				if sr.Blocked.Reason != "" {
					reason = sr.Blocked.Reason
				}
				errData, _ := json.Marshal(map[string]any{
					"error": map[string]string{"type": "guardrail_blocked", "message": reason},
				})
				fmt.Fprintf(c.Writer, "data: %s\n\n", errData)
				fmt.Fprintln(c.Writer, "data: [DONE]")
				fmt.Fprintln(c.Writer)
				flusher.Flush()
				// Upstream tokens were already consumed — record usage before returning.
				outTok := outputTokens
				if outTok == 0 {
					outTok = outputEstimate
				}
				go h.logUsage(c.GetHeader("X-Session-ID"), orgID, openaiReq, inputTokens, outTok,
					result.Route, pn, time.Since(start).Milliseconds(), result.FallbackCount, totalRetries,
					true, sr.Blocked.RuleName, modelRespBuf.String())
				return
			}
			chunk := sr.Chunk
			if chunk == nil || chunk.Done {
				if chunk != nil && chunk.Done {
					gotDone = true
				}
				continue
			}
			data, err := json.Marshal(chunk.Chunk)
			if err != nil {
				slog.Warn("playground: marshal stream chunk failed", "error", err)
				continue
			}
			fmt.Fprintf(c.Writer, "data: %s\n\n", data)
			flusher.Flush()
			if chunk.Chunk.Usage != nil {
				inputTokens = chunk.Chunk.Usage.PromptTokens
				outputTokens = chunk.Chunk.Usage.CompletionTokens
			}
		if len(chunk.Chunk.Choices) > 0 {
				content := chunk.Chunk.Choices[0].Delta.Content
				if content != "" {
					outputEstimate += token.Estimate(content)
					modelRespBuf.WriteString(content)
				}
			}
		}
	} else {
	plainLoop:
		for chunk := range result.StreamCh {
			select {
			case <-ctxDone:
				slog.Info("playground: client disconnected during stream", "model", req.Model)
				break plainLoop
			default:
			}
			if chunk.Done {
				gotDone = true
				break
			}
			if chunk.Chunk == nil {
				continue
			}
			data, err := json.Marshal(chunk.Chunk)
			if err != nil {
				slog.Warn("playground: marshal stream chunk failed", "error", err)
				continue
			}
			fmt.Fprintf(c.Writer, "data: %s\n\n", data)
			flusher.Flush()
			if chunk.Chunk.Usage != nil {
				inputTokens = chunk.Chunk.Usage.PromptTokens
				outputTokens = chunk.Chunk.Usage.CompletionTokens
			}
			if len(chunk.Chunk.Choices) > 0 {
				content := chunk.Chunk.Choices[0].Delta.Content
				if content != "" {
					outputEstimate += token.Estimate(content)
					modelRespBuf.WriteString(content)
				}
			}
		}
	}

	// Graceful degradation: if provider disconnected mid-stream, send [DONE]
	if !gotDone {
		slog.Warn("playground: stream disconnected mid-stream", "model", req.Model)
		fmt.Fprintln(c.Writer, "data: [DONE]")
		fmt.Fprintln(c.Writer)
		flusher.Flush()
	}

	// Fallback to estimation if provider did not return usage
	if outputTokens == 0 && outputEstimate > 0 {
		outputTokens = outputEstimate
	}
	// Send usage metadata before [DONE]
	latencyMs := time.Since(start).Milliseconds()
	usageData, _ := json.Marshal(map[string]any{
		"type": "usage",
		"data": map[string]any{
			"input":      inputTokens,
			"output":     outputTokens,
			"latency_ms": latencyMs,
			"provider":   pn,
		},
	})
	fmt.Fprintf(c.Writer, "data: %s\n\n", usageData)
	flusher.Flush()

	fmt.Fprintln(c.Writer, "data: [DONE]")
	fmt.Fprintln(c.Writer)
	flusher.Flush()

	c.Set("input_tokens", inputTokens)
	c.Set("output_tokens", outputTokens)

	sessionID := c.GetHeader("X-Session-ID")
	grTriggered, grRule := guardrailContext(c)
	go h.logUsage(sessionID, orgID, openaiReq, inputTokens, outputTokens,
		result.Route, pn, latencyMs, result.FallbackCount, totalRetries, grTriggered, grRule, modelRespBuf.String())
}

func (h *PlaygroundHandler) logUsage(
	sessionID string, orgID int64, req *domain.OpenAIRequest,
	inputTokens, outputTokens int,
	route *router.RouteResult, providerName string,
	latencyMs int64, fallbackCount, retryCount int,
	guardrailTriggered bool, guardrailRule string,
	modelResponse string,
) {
	defer func() { recover() }()

	entry := &service.UsageEntry{
		RouteType:      "playground",
		ModelRequested: req.Model,
		ModelUsed:      route.ProviderModel,
		ProviderID:     route.ProviderRow.ID,
		OrgID:          orgID,
		InputTokens:    inputTokens,
		OutputTokens:   outputTokens,
		InputPrice:     route.InputPrice,
		OutputPrice:    route.OutputPrice,
		Currency:       route.Currency,
		StatusCode:     http.StatusOK,
		LatencyMs:      latencyMs,
		FallbackCount:  fallbackCount,
		RetryCount:     retryCount,
	}
	if guardrailTriggered {
		entry.GuardrailTriggered = true
		entry.GuardrailRule = guardrailRule
	}
	if h.usageSvc.IsContentLogEnabled() {
		entry.UserMessage = truncateMessages(req.Messages)
		entry.ModelResponse = truncateContent(modelResponse)
	}
	logCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h.usageSvc.Log(logCtx, entry)

	if h.latencySvc != nil {
		h.latencySvc.RecordLatency(logCtx, providerName, latencyMs)
	}
}

func truncateMessages(msgs []domain.OpenAIMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role == "user" {
			if s, ok := m.Content.(string); ok {
				b.WriteString(s)
				b.WriteByte('\n')
			}
		}
	}
	if b.Len() > 1000 {
		s := b.String()
		for len(s) > 1000 {
			_, size := utf8.DecodeLastRuneInString(s)
			s = s[:len(s)-size]
		}
		return s
	}
	return b.String()
}

const maxContentLen = 65536

func truncateContent(s string) string {
	if utf8.RuneCountInString(s) <= maxContentLen {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxContentLen])
}

func guardrailContext(c *gin.Context) (triggered bool, rule string) {
	if v, ok := c.Get("guardrail_triggered"); ok {
		if b, _ := v.(bool); b {
			triggered = true
		}
	}
	if v, ok := c.Get("guardrail_rule"); ok {
		if s, _ := v.(string); s != "" {
			rule = s
		}
	}
	return
}

// --- Multimodal handlers ---

type PlaygroundImageRequest struct {
	Model   string `json:"model" binding:"required"`
	Prompt  string `json:"prompt" binding:"required"`
	N       int    `json:"n,omitempty"`
	Size    string `json:"size,omitempty"`
	Quality string `json:"quality,omitempty"`
}

type PlaygroundTTSRequest struct {
	Model          string  `json:"model" binding:"required"`
	Input          string  `json:"input" binding:"required"`
	Voice          string  `json:"voice" binding:"required"`
	ResponseFormat string  `json:"response_format,omitempty"`
	Speed          float64 `json:"speed,omitempty"`
}

type PlaygroundVideoRequest struct {
	Model          string `json:"model" binding:"required"`
	Prompt         string `json:"prompt" binding:"required"`
	AspectRatio    string `json:"aspect_ratio,omitempty"`
	Duration       int    `json:"duration,omitempty"`
	ReferenceImage string `json:"reference_image,omitempty"`
}

func (h *PlaygroundHandler) ImageGenerate(c *gin.Context) {
	var req PlaygroundImageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorResp(c, http.StatusBadRequest, ErrModelRequired, "model and prompt are required")
		return
	}
	if req.N <= 0 {
		req.N = 1
	} else if req.N > 4 {
		req.N = 4
	}

	start := time.Now()
	c.Set("model", req.Model)

	orgID := c.GetInt64("org_id")
	routes, err := h.resolver.Resolve(c.Request.Context(), req.Model, orgID)
	if err != nil || len(routes) == 0 {
		errorResp(c, http.StatusNotFound, ErrNoRouteForModel, fmt.Sprintf("no available route for model: %s", req.Model))
		return
	}

// Session affinity intentionally skipped for playground — interactive testing does not need sticky sessions
	routes = service.ExpandFallbackRoutes(c.Request.Context(), h.resolver, routes, orgID)
	var imageRoutes []*router.RouteResult
	for _, r := range routes {
		if _, ok := r.Provider.(provider.ImageProvider); ok {
			imageRoutes = append(imageRoutes, r)
		}
	}
	if len(imageRoutes) == 0 {
		errorResp(c, http.StatusNotFound, ErrNoImageProvider, "no image provider available")
		return
	}

	config := service.ResolveFallbackConfig(imageRoutes)
	engine := service.NewFallbackEngine(h.health, config)

	var totalRetries int
	result := engine.ExecuteNonStream(c.Request.Context(), imageRoutes,
		func(ctx context.Context, route *router.RouteResult) (any, error) {
			ip := route.Provider.(provider.ImageProvider)
			imgReq := domain.ImageRequest{
				Model:   route.ProviderModel,
				Prompt:  req.Prompt,
				N:       req.N,
				Size:    req.Size,
				Quality: req.Quality,
			}
			pn := route.Provider.Name()
			if h.activeTracker != nil {
				h.activeTracker.Incr(ctx, pn)
			}
			retryCfg := route.RetryConfig
			if len(imageRoutes) > 1 {
				retryCfg.NumRetries = 0
			}
			var resp *domain.ImageResponse
			rr := provider.WithRetry(ctx, retryCfg, h.budget, func(attemptCtx context.Context) error {
				attemptCtx = upstream.WithProviderName(attemptCtx, route.Provider.Name())
				attemptCtx = upstream.WithProviderModel(attemptCtx, route.ProviderModel)
				attemptCtx = upstream.WithProviderBaseURL(attemptCtx, route.ProviderRow.BaseURL)
				var callErr error
				resp, callErr = ip.GenerateImage(attemptCtx, &imgReq, route.ProviderRow.APIKey)
				return callErr
			})
			totalRetries += rr.RetriesUsed
			if h.activeTracker != nil {
				h.activeTracker.Decr(ctx, pn)
			}
			if rr.Err != nil {
				return nil, rr.Err
			}
			return resp, nil
		},
	)

	if result.FinalError != nil {
		slog.Error("playground: image generation failed", "model", req.Model, "error", result.FinalError)
		c.JSON(mapPlaygroundErrorStatus(result.FinalError), gin.H{"error": safeProviderError(result.FinalError)})
		return
	}

	resp, ok := result.Response.(*domain.ImageResponse)
	if !ok || resp == nil {
		slog.Error("playground: unexpected image response type", "model", req.Model)
		errorResp(c, http.StatusBadGateway, ErrUnexpectedResponse, "unexpected response")
		return
	}
	route := result.Route
	pn := route.ProviderRow.Name

	// Fill-in: some providers silently ignore the n parameter and return
	// fewer images than requested. Generate the deficit in parallel.
	if req.N > 1 && len(resp.Data) < req.N {
		remaining := req.N - len(resp.Data)
		slog.Info("playground: filling image deficit",
			"model", req.Model, "requested", req.N, "received", len(resp.Data), "filling", remaining)

		ip := route.Provider.(provider.ImageProvider)
		apiKey := route.ProviderRow.APIKey

		fills := make([][]domain.ImageData, remaining)
		var wg sync.WaitGroup
		for i := 0; i < remaining; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				fillReq := &domain.ImageRequest{
					Model:   route.ProviderModel,
					Prompt:  req.Prompt,
					N:       1,
					Size:    req.Size,
					Quality: req.Quality,
				}
				fillResp, err := ip.GenerateImage(c.Request.Context(), fillReq, apiKey)
				if err != nil {
					slog.Warn("playground: fill-in image request failed", "error", err)
					return
				}
				fills[idx] = fillResp.Data
			}(i)
		}
		wg.Wait()

		for _, f := range fills {
			resp.Data = append(resp.Data, f...)
		}
	}

	latencyMs := time.Since(start).Milliseconds()

	pricing := service.ResolvePricing(route.ExtraConfig)
	if pricing.Price <= 0 {
		slog.Warn("playground: image model has no pricing configured (extra_config.pricing) — recorded as free", "model", req.Model)
	}
	var cost float64
	if pricing.Price > 0 {
		cost = pricing.Price * float64(req.N)
	}

	c.Set("provider", pn)
	c.Set("input_tokens", 0)
	c.Set("output_tokens", 0)
	c.Set("input_price", pricing.Price)
	c.Set("output_price", 0)
	c.Set("cost", cost)

	grTriggered, grRule := guardrailContext(c)
	go func() {
		defer func() { recover() }()
		bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		entry := &service.UsageEntry{
			RouteType:       "playground_image",
			ModelRequested:  req.Model,
			ModelUsed:       route.ProviderModel,
			ProviderID:      route.ProviderRow.ID,
			OrgID:           orgID,
			Currency:        route.Currency,
			StatusCode:      http.StatusOK,
			LatencyMs:       latencyMs,
			FallbackCount:   result.FallbackCount,
			RetryCount:      totalRetries,
			PrecomputedCost: cost,
		}
		if grTriggered {
			entry.GuardrailTriggered = true
			entry.GuardrailRule = grRule
		}
		h.usageSvc.Log(bgCtx, entry)
		if h.latencySvc != nil {
			h.latencySvc.RecordLatency(bgCtx, pn, latencyMs)
		}
	}()

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"images":   resp.Data,
			"model":    route.ProviderModel,
			"provider": pn,
			"cost":     cost,
		},
	})
}

func (h *PlaygroundHandler) TextToSpeech(c *gin.Context) {
	var req PlaygroundTTSRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorResp(c, http.StatusBadRequest, ErrModelRequired, "model, input, and voice are required")
		return
	}

	start := time.Now()
	c.Set("model", req.Model)

	orgID := c.GetInt64("org_id")
	routes, err := h.resolver.Resolve(c.Request.Context(), req.Model, orgID)
	if err != nil || len(routes) == 0 {
		errorResp(c, http.StatusNotFound, ErrNoRouteForModel, fmt.Sprintf("no available route for model: %s", req.Model))
		return
	}

	routes = service.ExpandFallbackRoutes(c.Request.Context(), h.resolver, routes, orgID)
	var capable []*router.RouteResult
	for _, r := range routes {
		if _, ok := r.Provider.(provider.AudioProvider); ok {
			capable = append(capable, r)
		}
	}
	if len(capable) == 0 {
		errorResp(c, http.StatusNotFound, ErrNoAudioProvider, "no audio provider available")
		return
	}

	var audioStream io.ReadCloser
	var contentType string
	var successRoute *router.RouteResult
	var lastErr error

	for _, route := range capable {
		if h.health != nil && !h.health.IsHealthy(route.Provider.Name()) {
			continue
		}
		ap, _ := route.Provider.(provider.AudioProvider)
		pn := route.Provider.Name()
		if h.activeTracker != nil {
			h.activeTracker.Incr(c.Request.Context(), pn)
		}
		speechReq := domain.SpeechRequest{
			Model:          route.ProviderModel,
			Input:          req.Input,
			Voice:          req.Voice,
			ResponseFormat: req.ResponseFormat,
			Speed:          req.Speed,
		}
		stream, ct, err := ap.CreateSpeech(c.Request.Context(), &speechReq, route.ProviderRow.APIKey)
		if err != nil {
			if h.activeTracker != nil {
				h.activeTracker.Decr(c.Request.Context(), pn)
			}
			lastErr = err
			if h.health != nil {
				h.health.RecordFailure(route.Provider.Name())
			}
			if !provider.IsRetryableError(err) {
				break
			}
			continue
		}
		audioStream = stream
		contentType = ct
		successRoute = route
		break
	}

	if audioStream == nil {
		status := http.StatusBadGateway
		if lastErr != nil {
			status = mapPlaygroundErrorStatus(lastErr)
		}
		errorResp(c, status, ErrTTSFailed, "TTS failed")
		return
	}
	defer audioStream.Close()

	audioBuf := new(bytes.Buffer)
	n, err := io.Copy(audioBuf, io.LimitReader(audioStream, maxTTSAudioSize+1))
	if err != nil {
		slog.Error("playground: failed to buffer TTS audio", "error", err)
		errorResp(c, http.StatusBadGateway, ErrFailedProcessAudio, "failed to process audio")
		return
	}
	if n > maxTTSAudioSize {
		slog.Error("playground: TTS audio exceeds size limit", "bytes", n)
		errorResp(c, http.StatusBadGateway, ErrAudioTooLarge, "audio too large for playground (max 25 MB)")
		return
	}

	if h.activeTracker != nil {
		h.activeTracker.Decr(context.Background(), successRoute.Provider.Name())
	}
	if h.health != nil {
		h.health.RecordSuccess(successRoute.Provider.Name())
	}

	latencyMs := time.Since(start).Milliseconds()
	pn := successRoute.ProviderRow.Name

	pricing := service.ResolvePricing(successRoute.ExtraConfig)
	if pricing.Price <= 0 {
		slog.Warn("playground: TTS model has no pricing configured (extra_config.pricing) — recorded as free", "model", req.Model)
	}
	var cost float64
	if pricing.Price > 0 {
		cost = pricing.Price * float64(utf8.RuneCountInString(req.Input))
	}

	c.Set("provider", pn)
	c.Set("input_tokens", 0)
	c.Set("output_tokens", 0)
	c.Set("input_price", pricing.Price)
	c.Set("output_price", 0)
	c.Set("cost", cost)

	audioBase64 := "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(audioBuf.Bytes())

	grTriggered, grRule := guardrailContext(c)
	go func() {
		defer func() { recover() }()
		bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		entry := &service.UsageEntry{
			RouteType:       "playground_audio",
			ModelRequested:  req.Model,
			ModelUsed:       successRoute.ProviderModel,
			ProviderID:      successRoute.ProviderRow.ID,
			OrgID:           orgID,
			Currency:        successRoute.Currency,
			StatusCode:      http.StatusOK,
			LatencyMs:       latencyMs,
			PrecomputedCost: cost,
		}
		if grTriggered {
			entry.GuardrailTriggered = true
			entry.GuardrailRule = grRule
		}
		h.usageSvc.Log(bgCtx, entry)
		if h.latencySvc != nil {
			h.latencySvc.RecordLatency(bgCtx, pn, latencyMs)
		}
	}()

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"audio":       audioBase64,
			"content_type": contentType,
			"model":       successRoute.ProviderModel,
			"provider":    pn,
			"cost":        cost,
		},
	})
}

func (h *PlaygroundHandler) VideoGenerate(c *gin.Context) {
	var req PlaygroundVideoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorResp(c, http.StatusBadRequest, ErrModelRequired, "model and prompt are required")
		return
	}

	// Validate reference image size: base64 encoded 10MB ≈ 13.3M characters
	if len(req.ReferenceImage) > 13_300_000 {
		errorResp(c, http.StatusBadRequest, ErrVideoTooLarge, "reference image exceeds size limit (max 10 MB)")
		return
	}

	c.Set("model", req.Model)

	orgID := c.GetInt64("org_id")
	routes, err := h.resolver.Resolve(c.Request.Context(), req.Model, orgID)
	if err != nil || len(routes) == 0 {
		errorResp(c, http.StatusNotFound, ErrNoRouteForModel, fmt.Sprintf("no available route for model: %s", req.Model))
		return
	}

	routes = service.ExpandFallbackRoutes(c.Request.Context(), h.resolver, routes, orgID)
	var videoRoutes []*router.RouteResult
	for _, r := range routes {
		if _, ok := r.Provider.(provider.VideoProvider); ok {
			videoRoutes = append(videoRoutes, r)
		}
	}
	if len(videoRoutes) == 0 {
		errorResp(c, http.StatusServiceUnavailable, ErrNoVideoProvider, "no video provider available")
		return
	}

	config := service.ResolveFallbackConfig(videoRoutes)
	engine := service.NewFallbackEngine(h.health, config)

	var totalRetries int
	result := engine.ExecuteNonStream(c.Request.Context(), videoRoutes,
		func(ctx context.Context, route *router.RouteResult) (any, error) {
			vp := route.Provider.(provider.VideoProvider)
			videoReq := domain.VideoRequest{
				Model:          route.ProviderModel,
				Prompt:         req.Prompt,
				AspectRatio:    req.AspectRatio,
				Duration:       req.Duration,
				ReferenceImage: req.ReferenceImage,
			}
			pn := route.Provider.Name()
			if h.activeTracker != nil {
				h.activeTracker.Incr(ctx, pn)
			}
			retryCfg := route.RetryConfig
			if len(videoRoutes) > 1 {
				retryCfg.NumRetries = 0
			}
			var task *domain.VideoTask
			rr := provider.WithRetry(ctx, retryCfg, h.budget, func(attemptCtx context.Context) error {
				attemptCtx = upstream.WithProviderName(attemptCtx, route.Provider.Name())
				attemptCtx = upstream.WithProviderModel(attemptCtx, route.ProviderModel)
				attemptCtx = upstream.WithProviderBaseURL(attemptCtx, route.ProviderRow.BaseURL)
				var callErr error
				task, callErr = vp.SubmitVideoTask(attemptCtx, &videoReq, route.ProviderRow.APIKey)
				return callErr
			})
			totalRetries += rr.RetriesUsed
			if h.activeTracker != nil {
				h.activeTracker.Decr(ctx, pn)
			}
			if rr.Err != nil {
				return nil, rr.Err
			}
			return task, nil
		},
	)

	if result.FinalError != nil {
		slog.Error("playground: video task submission failed", "model", req.Model, "error", result.FinalError)
		c.JSON(mapPlaygroundErrorStatus(result.FinalError), gin.H{"error": safeProviderError(result.FinalError)})
		return
	}

	task, ok := result.Response.(*domain.VideoTask)
	if !ok || task == nil {
		slog.Error("playground: unexpected video task response type", "model", req.Model)
		errorResp(c, http.StatusBadGateway, ErrUnexpectedResponse, "unexpected response")
		return
	}

	route := result.Route
	pn := route.ProviderRow.Name

	// Store task mapping via shared VideoTaskService
	gwTaskID, err := h.taskSvc.SubmitTask(c.Request.Context(), service.VideoSubmitParams{
		UpstreamTaskID: task.TaskID,
		ProviderName:   pn,
		APIKey:         route.ProviderRow.APIKey,
		Model:          task.Model,
		OrgID:          orgID,
		APIKeyID:       0, // Playground: no API key
		Currency:       route.Currency,
		InputPrice:     route.InputPrice,
		Prompt:         truncateStr(req.Prompt, 200),
	})
	if err != nil {
		slog.Error("playground: failed to store video task", "error", err)
		errorResp(c, http.StatusInternalServerError, ErrUnexpectedResponse, "failed to store task")
		return
	}

	// Replace upstream task ID with gateway task ID for polling
	task.TaskID = gwTaskID

	c.Set("provider", pn)
	c.Set("input_tokens", 0)
	c.Set("output_tokens", 0)

	c.JSON(http.StatusOK, gin.H{
		"data": task,
	})
}

func (h *PlaygroundHandler) GetVideoStatus(c *gin.Context) {
	taskID := c.Param("taskId")
	if taskID == "" {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "taskId is required")
		return
	}

	orgID := c.GetInt64("org_id")

	// Delegate to shared VideoTaskService (handles provider lookup, billing dedup, tenant isolation)
	task, _, err := h.taskSvc.GetTask(c.Request.Context(), taskID, orgID)
	if err != nil {
		if errors.Is(err, service.ErrTaskNotFound) {
			errorResp(c, http.StatusNotFound, ErrVideoTaskNotFound, "video task not found or expired")
			return
		}
		slog.Error("playground: video status check failed", "taskId", taskID, "error", err)
		c.JSON(mapPlaygroundErrorStatus(err), gin.H{"error": safeProviderError(err)})
		return
	}

	// Set provider context for usage middleware (Playground already uses VideoTaskService billing)
	if task.ProviderName != "" {
		c.Set("provider", task.ProviderName)
	}
	c.Set("input_tokens", 0)
	c.Set("output_tokens", 0)

	c.JSON(http.StatusOK, gin.H{
		"data": task,
	})
}

func (h *PlaygroundHandler) Transcribe(c *gin.Context) {
	h.handleTranscribeOrTranslate(c, false)
}

func (h *PlaygroundHandler) Translate(c *gin.Context) {
	h.handleTranscribeOrTranslate(c, true)
}

func (h *PlaygroundHandler) handleTranscribeOrTranslate(c *gin.Context, isTranslate bool) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		errorResp(c, http.StatusBadRequest, ErrFileRequired, "file field required")
		return
	}
	defer file.Close()

	fileData, err := io.ReadAll(io.LimitReader(file, maxRequestBody))
	if err != nil {
		errorResp(c, http.StatusBadRequest, ErrFailedReadFile, "failed to read file")
		return
	}

	req := domain.TranscriptionRequest{
		FileData:       fileData,
		FileName:       header.Filename,
		Model:          c.PostForm("model"),
		Language:       c.PostForm("language"),
		Prompt:         c.PostForm("prompt"),
		ResponseFormat: c.PostForm("response_format"),
	}
	if tempStr := c.PostForm("temperature"); tempStr != "" {
		if temp, err := strconv.ParseFloat(tempStr, 64); err == nil {
			req.Temperature = temp
		}
	}
	if req.Model == "" {
		errorResp(c, http.StatusBadRequest, ErrModelRequired, "model field required")
		return
	}

	start := time.Now()
	c.Set("model", req.Model)

	orgID := c.GetInt64("org_id")
	routes, err := h.resolver.Resolve(c.Request.Context(), req.Model, orgID)
	if err != nil || len(routes) == 0 {
		errorResp(c, http.StatusNotFound, ErrNoRouteForModel, fmt.Sprintf("no available route for model: %s", req.Model))
		return
	}

	routes = service.ExpandFallbackRoutes(c.Request.Context(), h.resolver, routes, orgID)
	var capable []*router.RouteResult
	for _, r := range routes {
		if _, ok := r.Provider.(provider.AudioProvider); ok {
			capable = append(capable, r)
		}
	}
	if len(capable) == 0 {
		errorResp(c, http.StatusNotFound, ErrNoAudioProvider, "no audio provider available")
		return
	}

	config := service.ResolveFallbackConfig(capable)
	engine := service.NewFallbackEngine(h.health, config)

	var totalRetries int
	result := engine.ExecuteNonStream(c.Request.Context(), capable,
		func(ctx context.Context, route *router.RouteResult) (any, error) {
			ap := route.Provider.(provider.AudioProvider)
			reqCopy := req
			reqCopy.Model = route.ProviderModel
			// Billing needs the audio duration, which providers only return in
			// verbose_json. The playground builds its own response envelope, so
			// switching the internal format is invisible to the client.
			if reqCopy.ResponseFormat == "" || reqCopy.ResponseFormat == "json" {
				reqCopy.ResponseFormat = "verbose_json"
			}
			pn := route.Provider.Name()
			if h.activeTracker != nil {
				h.activeTracker.Incr(ctx, pn)
			}
			retryCfg := route.RetryConfig
			if len(capable) > 1 {
				retryCfg.NumRetries = 0
			}
			var resp *domain.TranscriptionResponse
			rr := provider.WithRetry(ctx, retryCfg, h.budget, func(attemptCtx context.Context) error {
				attemptCtx = upstream.WithProviderName(attemptCtx, route.Provider.Name())
				attemptCtx = upstream.WithProviderModel(attemptCtx, route.ProviderModel)
				attemptCtx = upstream.WithProviderBaseURL(attemptCtx, route.ProviderRow.BaseURL)
				var callErr error
				if isTranslate {
					resp, callErr = ap.Translate(attemptCtx, &reqCopy, route.ProviderRow.APIKey)
				} else {
					resp, callErr = ap.Transcribe(attemptCtx, &reqCopy, route.ProviderRow.APIKey)
				}
				return callErr
			})
			totalRetries += rr.RetriesUsed
			if h.activeTracker != nil {
				h.activeTracker.Decr(ctx, pn)
			}
			if rr.Err != nil {
				return nil, rr.Err
			}
			return resp, nil
		},
	)

	if result.FinalError != nil {
		action := "transcription"
		if isTranslate {
			action = "translation"
		}
		slog.Error("playground: audio "+action+" failed", "model", req.Model, "error", result.FinalError)
		c.JSON(mapPlaygroundErrorStatus(result.FinalError), gin.H{"error": safeProviderError(result.FinalError)})
		return
	}

	resp, ok := result.Response.(*domain.TranscriptionResponse)
	if !ok || resp == nil {
		slog.Error("playground: unexpected transcription response type", "model", req.Model)
		errorResp(c, http.StatusBadGateway, ErrUnexpectedResponse, "unexpected response")
		return
	}
	route := result.Route
	latencyMs := time.Since(start).Milliseconds()
	pn := route.ProviderRow.Name

	pricing := service.ResolvePricing(route.ExtraConfig)
	if pricing.Price <= 0 {
		slog.Warn("playground: transcription model has no pricing configured (extra_config.pricing) — recorded as free", "model", req.Model)
	}
	var cost float64
	if pricing.Price > 0 {
		if resp.Duration > 0 {
			cost = pricing.Price * resp.Duration
		} else {
			slog.Warn("playground: transcription returned no duration — cost recorded as 0 (pricing is per second)", "model", req.Model)
		}
	}

	c.Set("provider", pn)
	c.Set("input_price", pricing.Price)
	c.Set("input_tokens", 0)
	c.Set("output_tokens", 0)
	c.Set("output_price", 0)
	c.Set("cost", cost)

	// Response-side guardrail for transcribed text
	if h.guardrailSvc != nil && h.guardrailSvc.IsEnabled() && resp.Text != "" {
		grResult, grErr := h.guardrailSvc.Check(c.Request.Context(), &guardrail.CheckRequest{
			Content:   resp.Text,
			Direction: guardrail.DirectionResponse,
			Model:     req.Model,
			APIKeyID:  0,
			TeamID:    0,
			OrgID:     0,
		})
		if grErr != nil {
			slog.Warn("playground: guardrail STT response check failed", "error", grErr)
		} else if grResult != nil && grResult.Blocked {
			if grResult.Action == "block" {
				reason := "blocked by guardrail"
				if grResult.Reason != "" {
					reason = grResult.Reason
				}
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "guardrail_blocked", "message": reason}})
				return
			}
			if grResult.Action == "mask" && grResult.MaskedContent != "" {
				c.Set("guardrail_triggered", true)
				c.Set("guardrail_rule", grResult.RuleName)
				resp.Text = grResult.MaskedContent
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

	grTriggered, grRule := guardrailContext(c)
	go func() {
		defer func() { recover() }()
		bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		h.usageSvc.Log(bgCtx, &service.UsageEntry{
			RouteType:       "playground_audio",
			ModelRequested:  req.Model,
			ModelUsed:       route.ProviderModel,
			ProviderID:      route.ProviderRow.ID,
			OrgID:           orgID,
			Currency:        route.Currency,
			StatusCode:      http.StatusOK,
			LatencyMs:       latencyMs,
			FallbackCount:   result.FallbackCount,
			RetryCount:      totalRetries,
			PrecomputedCost: cost,
			GuardrailTriggered: grTriggered,
			GuardrailRule:      grRule,
		})
		if h.latencySvc != nil {
			h.latencySvc.RecordLatency(bgCtx, pn, latencyMs)
		}
	}()

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"text":     resp.Text,
			"language": resp.Language,
			"duration": resp.Duration,
			"segments": resp.Segments,
			"model":    route.ProviderModel,
			"provider": pn,
			"cost":     cost,
		},
	})
}
