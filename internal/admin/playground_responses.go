package admin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/debug/upstream"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	"github.com/crosslink/internal/translator"
)

// PlaygroundResponsesRequest models the admin playground Responses request.
// Input is polymorphic (string or []item); 3A forwards the body verbatim,
// 3B translates to internal OpenAI.
type PlaygroundResponsesRequest struct {
	Model              string          `json:"model"`
	Input             json.RawMessage `json:"input"`
	Instructions      string          `json:"instructions,omitempty"`
	MaxOutputTokens   *int            `json:"max_output_tokens,omitempty"`
	PreviousResponseID string         `json:"previous_response_id,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
}

type pgResponsesRaw struct {
	raw    []byte
	status int
}

// pgRouteSupportsResponses mirrors the gateway dispatch: 3A requires BOTH the
// config flag and the ResponsesProvider interface.
func pgRouteSupportsResponses(r *router.RouteResult) bool {
	if !translator.SupportsResponses(r.ExtraConfig) {
		return false
	}
	_, ok := r.Provider.(provider.ResponsesProvider)
	return ok
}

// Responses exercises the 3A (raw passthrough) / 3B (translation) dispatch for
// the Responses API, for admin interactive testing.
func (h *PlaygroundHandler) Responses(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxRequestBody))
	if err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "failed to read body")
		return
	}
	var req PlaygroundResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "invalid request")
		return
	}
	if req.Model == "" || len(req.Input) == 0 {
		errorResp(c, http.StatusBadRequest, ErrModelRequired, "model and input are required")
		return
	}

	start := time.Now()
	orgID := c.GetInt64("org_id")
	routes, err := h.resolver.Resolve(c.Request.Context(), req.Model, orgID)
	if err != nil || len(routes) == 0 {
		errorResp(c, http.StatusNotFound, ErrNoRouteForModel, fmt.Sprintf("no available route for model: %s", req.Model))
		return
	}

	routes = service.ExpandFallbackRoutes(c.Request.Context(), h.resolver, routes, orgID)

	// Stateful (previous_response_id) requests can only be served by 3A routes.
	if req.PreviousResponseID != "" {
		var onlyA []*router.RouteResult
		for _, r := range routes {
			if pgRouteSupportsResponses(r) {
				onlyA = append(onlyA, r)
			}
		}
		if len(onlyA) == 0 {
			errorResp(c, http.StatusPreconditionFailed, ErrNoRouteForModel, "this model does not support session continuation (previous_response_id)")
			return
		}
		routes = onlyA
	}

	if req.Stream {
		h.responsesStream(c, body, &req, routes, start)
		return
	}
	h.responsesNonStream(c, body, &req, routes, start)
}

func (h *PlaygroundHandler) responsesNonStream(c *gin.Context, rawBody []byte, req *PlaygroundResponsesRequest, routes []*router.RouteResult, start time.Time) {
	config := service.ResolveFallbackConfig(routes)
	engine := service.NewFallbackEngine(h.health, config)

	result := engine.ExecuteNonStream(c.Request.Context(), routes, func(ctx context.Context, route *router.RouteResult) (any, error) {
		pn := route.Provider.Name()
		if h.activeTracker != nil {
			h.activeTracker.Incr(ctx, pn)
		}
		retryCfg := route.RetryConfig
		if len(routes) > 1 {
			retryCfg.NumRetries = 0
		}
		var resp any
		rr := provider.WithRetry(ctx, retryCfg, h.budget, func(attemptCtx context.Context) error {
			attemptCtx = upstream.WithProviderName(attemptCtx, pn)
			attemptCtx = upstream.WithProviderModel(attemptCtx, route.ProviderModel)
			attemptCtx = upstream.WithProviderBaseURL(attemptCtx, route.ProviderRow.BaseURL)
			if pgRouteSupportsResponses(route) {
				rp := route.Provider.(provider.ResponsesProvider)
				rc, status, err := rp.Responses(attemptCtx, rawBody, route.ProviderRow.APIKey)
				if err != nil {
					return err
				}
				defer rc.Close()
				buf, err := io.ReadAll(rc)
				if err != nil {
					return err
				}
				if status >= 400 {
					return fmt.Errorf("upstream responses status %d", status)
				}
				resp = &pgResponsesRaw{raw: buf, status: status}
				return nil
			}
			// 3B
			oaiReq, err := translator.ResponsesToOpenAI(&domain.ResponsesRequest{
				Model: req.Model, Input: req.Input, Instructions: req.Instructions,
				MaxOutputTokens: req.MaxOutputTokens, Temperature: req.Temperature, TopP: req.TopP,
			})
			if err != nil {
				return err
			}
			oaiReq.Model = route.ProviderModel
			oaiResp, err := route.Provider.Chat(attemptCtx, oaiReq, route.ProviderRow.APIKey)
			if err != nil {
				return err
			}
			resp = translator.OpenAIToResponses(oaiResp, req.Model)
			return nil
		})
		if h.activeTracker != nil {
			h.activeTracker.Decr(context.Background(), pn)
		}
		return resp, rr.Err
	})

	if result.FinalError != nil {
		slog.Error("playground: responses failed", "model", req.Model, "error", result.FinalError)
		statusCode := mapPlaygroundErrorStatus(result.FinalError)
		c.JSON(statusCode, gin.H{"error": fmt.Sprintf("responses failed: %v", result.FinalError)})
		return
	}
	route := result.Route
	latency := time.Since(start).Milliseconds()

	switch r := result.Response.(type) {
	case *pgResponsesRaw:
		h.logResponsesUsage(req, route, route.Provider.Name(), latency, result.FallbackCount, 0, r.raw)
		c.Data(r.status, "application/json", r.raw)
	case *domain.ResponsesResponse:
		h.logResponsesUsage(req, route, route.Provider.Name(), latency, result.FallbackCount, 0, nil)
		c.JSON(http.StatusOK, r)
	default:
		c.JSON(http.StatusBadGateway, gin.H{"error": "unexpected response type"})
	}
}

func (h *PlaygroundHandler) responsesStream(c *gin.Context, rawBody []byte, req *PlaygroundResponsesRequest, routes []*router.RouteResult, start time.Time) {
	var aRoutes, bRoutes []*router.RouteResult
	for _, r := range routes {
		if pgRouteSupportsResponses(r) {
			aRoutes = append(aRoutes, r)
		} else {
			bRoutes = append(bRoutes, r)
		}
	}

	// 3A raw stream with connect-level fallback.
	for _, route := range aRoutes {
		rp := route.Provider.(provider.ResponsesProvider)
		ctx := upstream.WithProviderName(c.Request.Context(), route.Provider.Name())
		rc, status, err := rp.Responses(ctx, rawBody, route.ProviderRow.APIKey)
		if err != nil || status >= 400 {
			if rc != nil {
				rc.Close()
			}
			continue
		}
		h.writeStreamHeader(c)
		pgCopySSE(c, rc)
		rc.Close()
		h.logResponsesUsage(req, route, route.Provider.Name(), time.Since(start).Milliseconds(), 0, 0, nil)
		return
	}

	if len(bRoutes) == 0 {
		errorResp(c, http.StatusBadGateway, ErrNoRouteForModel, "no streaming responses provider available")
		return
	}
	config := service.ResolveFallbackConfig(bRoutes)
	engine := service.NewFallbackEngine(h.health, config)
	builder := translator.NewResponsesStreamBuilder("", req.Model)

	res := engine.ExecuteStream(c.Request.Context(), bRoutes, func(ctx context.Context, route *router.RouteResult) (<-chan domain.SSEChunk, error) {
		oaiReq, err := translator.ResponsesToOpenAI(&domain.ResponsesRequest{
			Model: req.Model, Input: req.Input, Instructions: req.Instructions,
			MaxOutputTokens: req.MaxOutputTokens, Temperature: req.Temperature, TopP: req.TopP,
		})
		if err != nil {
			return nil, err
		}
		oaiReq.Model = route.ProviderModel
		oaiReq.Stream = true
		return route.Provider.StreamChat(ctx, oaiReq, route.ProviderRow.APIKey)
	})
	if res.FinalError != nil {
		errorResp(c, http.StatusBadGateway, ErrNoRouteForModel, fmt.Sprintf("responses stream failed: %v", res.FinalError))
		return
	}
	route := res.Route
	h.writeStreamHeader(c)
	pgWriteEvents(c, builder.Start())
	var lastUsage domain.OpenAIUsage
	var finishReason string
	for chunk := range res.StreamCh {
		if chunk.Done {
			break
		}
		if chunk.Chunk != nil {
			if len(chunk.Chunk.Choices) > 0 && chunk.Chunk.Choices[0].FinishReason != nil {
				finishReason = *chunk.Chunk.Choices[0].FinishReason
			}
			if chunk.Chunk.Usage != nil {
				lastUsage = domain.OpenAIUsage{PromptTokens: chunk.Chunk.Usage.PromptTokens, CompletionTokens: chunk.Chunk.Usage.CompletionTokens, TotalTokens: chunk.Chunk.Usage.TotalTokens}
			}
			pgWriteEvents(c, builder.Next(chunk.Chunk))
		}
	}
	pgWriteEvents(c, builder.Finish(lastUsage, finishReason, req.Model))
	c.Writer.Write([]byte("data: [DONE]\n\n"))
	c.Writer.Flush()
	h.logResponsesUsage(req, route, route.Provider.Name(), time.Since(start).Milliseconds(), 0, 0, nil)
}

func (h *PlaygroundHandler) writeStreamHeader(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Status(http.StatusOK)
}

func pgWriteEvents(c *gin.Context, events []domain.ResponsesEvent) {
	flusher, _ := c.Writer.(http.Flusher)
	for _, ev := range events {
		data, err := json.Marshal(ev.Payload)
		if err != nil {
			continue
		}
		fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", ev.Type, data)
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// pgCopySSE copies upstream SSE bytes verbatim to the client.
func pgCopySSE(c *gin.Context, rc io.Reader) {
	flusher, _ := c.Writer.(http.Flusher)
	br := bufio.NewReader(rc)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			c.Writer.WriteString(line)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func pgPeekResponseID(raw []byte) string {
	var v struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &v) == nil {
		return v.ID
	}
	return ""
}

func (h *PlaygroundHandler) logResponsesUsage(req *PlaygroundResponsesRequest, route *router.RouteResult, providerName string, latencyMs int64, fallbackCount, retryCount int, rawResp []byte) {
	// Translate to OpenAI request shape for content logging consistency.
	oaiReq, _ := translator.ResponsesToOpenAI(&domain.ResponsesRequest{
		Model: req.Model, Input: req.Input, Instructions: req.Instructions,
	})
	respSummary := ""
	if len(rawResp) > 0 {
		respSummary = string(rawResp)
	}
	h.logUsage("", oaiReq, 0, 0, route, providerName, latencyMs, fallbackCount, retryCount, false, "", respSummary)
}
