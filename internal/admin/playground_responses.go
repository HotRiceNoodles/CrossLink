package admin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/crosslink/internal/debug/upstream"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	"github.com/crosslink/internal/translator"
	"github.com/gin-gonic/gin"
)

// PlaygroundResponsesRequest models the admin playground Responses request.
// Input is polymorphic (string or []item); 3A forwards the body verbatim,
// 3B translates to internal OpenAI.
type PlaygroundResponsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       string          `json:"instructions,omitempty"`
	MaxOutputTokens    *int            `json:"max_output_tokens,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"top_p,omitempty"`
}

type pgResponsesRaw struct {
	raw    []byte
	status int
}

// maxUpstreamResponseRead caps upstream Responses body reads (matches the
// provider package's response-size cap).
const maxUpstreamResponseRead = 50 << 20 // 50 MB

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
				buf, err := io.ReadAll(io.LimitReader(rc, maxUpstreamResponseRead))
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
		// Response-direction check on the raw body BEFORE it is delivered.
		if h.guardrailSvc != nil && h.guardrailSvc.IsEnabled() && pgResponsesGuardBlocked(c, h.guardrailSvc, req.Model, pgExtractResponsesText(r.raw)) {
			h.logResponsesUsage(c, req, route, route.Provider.Name(), latency, result.FallbackCount, 0, nil)
			return
		}
		h.logResponsesUsage(c, req, route, route.Provider.Name(), latency, result.FallbackCount, 0, r.raw)
		c.Data(r.status, "application/json", r.raw)
	case *domain.ResponsesResponse:
		if buf, err := json.Marshal(r); err == nil {
			if h.guardrailSvc != nil && h.guardrailSvc.IsEnabled() && pgResponsesGuardBlocked(c, h.guardrailSvc, req.Model, pgExtractResponsesText(buf)) {
				h.logResponsesUsage(c, req, route, route.Provider.Name(), latency, result.FallbackCount, 0, nil)
				return
			}
		}
		h.logResponsesUsage(c, req, route, route.Provider.Name(), latency, result.FallbackCount, 0, nil)
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
		pgCopySSEGuarded(c, rc, h.guardrailSvc, req.Model)
		rc.Close()
		h.logResponsesUsage(c, req, route, route.Provider.Name(), time.Since(start).Milliseconds(), 0, 0, nil)
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
	if res.CancelStream != nil {
		defer res.CancelStream()
	}
	h.writeStreamHeader(c)

	// Strict mode: buffer every event (bytes and event count capped) and run
	// the final check BEFORE anything is released. Window mode: emit as it
	// arrives and still run the end-of-stream check for observability —
	// already-emitted window content is not retractable (documented tradeoff).
	strict := h.guardrailSvc != nil && h.guardrailSvc.IsEnabled() && h.guardrailSvc.StreamMode() == "strict"
	var held []string
	var text strings.Builder
	heldBytes, heldEvents := 0, 0
	overflow := false
	emit := func(events []domain.ResponsesEvent) {
		flusher, _ := c.Writer.(http.Flusher)
		for _, ev := range events {
			data, err := json.Marshal(ev.Payload)
			if err != nil {
				continue
			}
			text.WriteString(pgExtractResponsesText(data))
			block := fmt.Sprintf("event: %s\ndata: %s\n\n", ev.Type, data)
			if strict {
				held = append(held, block)
				heldBytes += len(block)
				heldEvents++
				if heldBytes > h.guardrailSvc.StrictMaxBytes() || heldEvents > h.guardrailSvc.StrictMaxEvents() {
					overflow = true
				}
				continue
			}
			c.Writer.WriteString(block)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if !strict && len(events) > 0 {
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	capFail := func() {
		c.Set("guardrail_triggered", true)
		c.Set("guardrail_rule", "guardrail_stream_cap")
		fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", `{"type":"guardrail_blocked","message":"streamed response exceeds strict-mode guardrail buffer limit"}`)
		if f, _ := c.Writer.(http.Flusher); f != nil {
			f.Flush()
		}
	}

	emit(builder.Start())
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
			emit(builder.Next(chunk.Chunk))
		}
		if overflow {
			break
		}
	}
	if !overflow {
		emit(builder.Finish(lastUsage, finishReason, req.Model))
	}
	if overflow {
		capFail()
		h.logResponsesUsage(c, req, route, route.Provider.Name(), time.Since(start).Milliseconds(), 0, 0, nil)
		return
	}
	doneBlock := "data: [DONE]\n\n"
	if strict {
		held = append(held, doneBlock)
	} else {
		c.Writer.WriteString(doneBlock)
		if f, _ := c.Writer.(http.Flusher); f != nil {
			f.Flush()
		}
	}
	if h.guardrailSvc != nil && h.guardrailSvc.IsEnabled() {
		grCtx, grCancel := guardResponseCtx(c, h.guardrailSvc)
		grResult, grErr := h.guardrailSvc.Check(grCtx, &guardrail.CheckRequest{Content: text.String(), Direction: guardrail.DirectionResponse, Model: req.Model})
		grCancel()
		if grErr != nil {
			if h.guardrailSvc.IsFailOpen() {
				slog.Warn("playground: responses stream guardrail check failed, fail-open", "error", grErr)
			} else {
				// Fail-closed: the strict buffer must NOT be released and the
				// stream terminates with a guardrail error.
				slog.Error("playground: responses stream guardrail check failed", "error", grErr)
				c.Set("guardrail_triggered", true)
				c.Set("guardrail_rule", "guardrail_unavailable")
				fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", `{"type":"guardrail_error","message":"guardrail service unavailable"}`)
				if f, _ := c.Writer.(http.Flusher); f != nil {
					f.Flush()
				}
				h.logResponsesUsage(c, req, route, route.Provider.Name(), time.Since(start).Milliseconds(), 0, 0, nil)
				return
			}
		}
		if grResult != nil && grResult.Truncated {
			c.Set("guardrail_truncated", true)
		}
		if grErr == nil && grResult != nil && grResult.Blocked {
			// Strict: nothing was released — withhold the buffer and emit only
			// the guardrail error. Window: content already emitted; the error
			// event still terminates the stream for the client.
			c.Set("guardrail_triggered", true)
			c.Set("guardrail_rule", grResult.RuleName)
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", `{"type":"guardrail_blocked","message":"blocked by guardrail"}`)
			if f, _ := c.Writer.(http.Flusher); f != nil {
				f.Flush()
			}
			h.logResponsesUsage(c, req, route, route.Provider.Name(), time.Since(start).Milliseconds(), 0, 0, nil)
			return
		}
		if grErr == nil && grResult != nil && grResult.Action == "log" {
			c.Set("guardrail_triggered", true)
			c.Set("guardrail_rule", grResult.RuleName)
		}
	}
	if strict {
		flusher, _ := c.Writer.(http.Flusher)
		for _, block := range held {
			c.Writer.WriteString(block)
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	h.logResponsesUsage(c, req, route, route.Provider.Name(), time.Since(start).Milliseconds(), 0, 0, nil)
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

// pgCopySSEGuarded relays upstream SSE with response-direction guardrail
// checks. Strict mode buffers all lines (byte and event caps, comments/blank
// lines count toward bytes) and releases them only after the final check;
// window mode passes through and end-checks for observability.
func pgCopySSEGuarded(c *gin.Context, rc io.Reader, svc *guardrail.GuardrailService, model string) {
	strict := svc != nil && svc.IsEnabled() && svc.StreamMode() == "strict"
	flusher, _ := c.Writer.(http.Flusher)
	br := bufio.NewReader(rc)
	var held []string
	var text strings.Builder
	heldBytes, heldEvents := 0, 0
	maxBytes, maxEvents := int(^uint(0)>>1), int(^uint(0)>>1)
	if strict {
		maxBytes = svc.StrictMaxBytes()
		maxEvents = svc.StrictMaxEvents()
	}
	overflow := false
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			if strict {
				held = append(held, line)
				heldBytes += len(line)
				if data, ok := pgSSEData(line); ok {
					heldEvents++
					text.WriteString(pgExtractResponsesText(data))
				}
				if heldBytes > maxBytes || heldEvents > maxEvents {
					overflow = true
				}
			} else {
				c.Writer.WriteString(line)
				if flusher != nil {
					flusher.Flush()
				}
				if data, ok := pgSSEData(line); ok {
					text.WriteString(pgExtractResponsesText(data))
				}
			}
		}
		if err != nil || overflow {
			break
		}
	}
	if overflow {
		c.Set("guardrail_triggered", true)
		c.Set("guardrail_rule", "guardrail_stream_cap")
		fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", `{"type":"guardrail_blocked","message":"streamed response exceeds strict-mode guardrail buffer limit"}`)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	if svc == nil || !svc.IsEnabled() {
		return
	}
	grCtx, grCancel := guardResponseCtx(c, svc)
	grResult, grErr := svc.Check(grCtx, &guardrail.CheckRequest{Content: text.String(), Direction: guardrail.DirectionResponse, Model: model})
	grCancel()
	if grErr != nil {
		if svc.IsFailOpen() {
			slog.Warn("playground: responses stream guardrail check failed, fail-open", "error", grErr)
		} else {
			// Fail-closed: withhold the strict buffer entirely — zero
			// unverified content is released.
			slog.Error("playground: responses stream guardrail check failed", "error", grErr)
			c.Set("guardrail_triggered", true)
			c.Set("guardrail_rule", "guardrail_unavailable")
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", `{"type":"guardrail_error","message":"guardrail service unavailable"}`)
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
	}
	if grResult != nil && grResult.Truncated {
		c.Set("guardrail_truncated", true)
	}
	if grErr == nil && grResult != nil && grResult.Blocked {
		c.Set("guardrail_triggered", true)
		c.Set("guardrail_rule", grResult.RuleName)
		if strict {
			// Nothing was released: withhold the buffer, emit only the error.
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", `{"type":"guardrail_blocked","message":"blocked by guardrail"}`)
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		// Window mode: content already emitted; terminate with the error.
		fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", `{"type":"guardrail_blocked","message":"blocked by guardrail"}`)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	if grErr == nil && grResult != nil && grResult.Action == "log" {
		c.Set("guardrail_triggered", true)
		c.Set("guardrail_rule", grResult.RuleName)
	}
	if strict {
		for _, block := range held {
			c.Writer.WriteString(block)
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// pgSSEData returns the JSON payload of a "data:" SSE line.
func pgSSEData(line string) ([]byte, bool) {
	if !strings.HasPrefix(line, "data:") {
		return nil, false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return nil, false
	}
	return []byte(payload), true
}

// pgResponsesGuardBlocked runs the response-direction check for a non-stream
// playground Responses body. It reports whether the body must NOT be
// delivered: policy block, or a mask that cannot be applied field-safely to
// the structured body (controlled rejection, never degrade to forwarding the
// original). log hits and partial coverage only set observability flags.
func pgResponsesGuardBlocked(c *gin.Context, svc *guardrail.GuardrailService, model, text string) bool {
	if text == "" {
		return false
	}
	grCtx, grCancel := guardResponseCtx(c, svc)
	defer grCancel()
	grResult, grErr := svc.Check(grCtx, &guardrail.CheckRequest{Content: text, Direction: guardrail.DirectionResponse, Model: model})
	if grErr != nil {
		// Check only returns an error when it could not verify AND policy is
		// fail-closed (fail-open is absorbed inside the service). Mirror the
		// /v1 handlers: reject rather than deliver unverified content.
		if svc.IsFailOpen() {
			slog.Warn("playground: responses guardrail check failed, fail-open", "error", grErr)
			return false
		}
		slog.Error("playground: responses guardrail check failed", "error", grErr)
		c.Set("guardrail_triggered", true)
		c.Set("guardrail_rule", "guardrail_unavailable")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"type": "guardrail_error", "message": "guardrail service unavailable"}})
		return true
	}
	if grResult == nil {
		return false
	}
	if grResult.Truncated {
		c.Set("guardrail_truncated", true)
	}
	if grResult.Blocked {
		c.Set("guardrail_triggered", true)
		c.Set("guardrail_rule", grResult.RuleName)
		reason := "blocked by guardrail"
		if grResult.Reason != "" {
			reason = grResult.Reason
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "guardrail_blocked", "message": reason}})
		return true
	}
	if grResult.Action == "mask" {
		// Whole-string MaskedContent cannot be written back into the
		// structured Responses body field-safely — refuse rather than leak.
		c.Set("guardrail_triggered", true)
		c.Set("guardrail_rule", grResult.RuleName)
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "guardrail_blocked", "message": "guardrail mask could not be applied safely"}})
		return true
	}
	if grResult.Action == "log" {
		c.Set("guardrail_triggered", true)
		c.Set("guardrail_rule", grResult.RuleName)
	}
	return false
}

// pgExtractResponsesText collects output-bearing string leaves from a
// Responses JSON payload (non-stream body or one SSE event), aligned with the
// gateway Responses gate's event family: text/arguments deltas,
// output_text.done / reasoning_text.done (top-level text),
// function_call_arguments.done (top-level arguments, canonicalized),
// content_part.done (part.text), output_item.done (item),
// response.completed (response.output), plus the non-stream body shape
// (output[] items with content[] parts and reasoning summary[] parts).
func pgExtractResponsesText(payload []byte) string {
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return ""
	}
	var sb strings.Builder
	if d, ok := root["delta"].(string); ok {
		sb.WriteString(d)
	}
	if a, ok := root["arguments"].(string); ok && a != "" {
		sb.WriteString(pgCanonicalJSON(a))
	}
	if s, ok := root["text"].(string); ok {
		sb.WriteString(s)
	}
	if part, ok := root["part"].(map[string]any); ok {
		if s, ok := part["text"].(string); ok {
			sb.WriteString(s)
		}
	}
	if item, ok := root["item"].(map[string]any); ok {
		pgWalkResponsesItem(item, &sb)
	}
	if resp, ok := root["response"].(map[string]any); ok {
		if out, ok := resp["output"].([]any); ok {
			pgWalkResponsesItems(out, &sb)
		}
		if cont, ok := resp["content"].([]any); ok {
			pgWalkResponsesContent(cont, &sb)
		}
	}
	if out, ok := root["output"].([]any); ok {
		pgWalkResponsesItems(out, &sb)
	}
	if cont, ok := root["content"].([]any); ok {
		pgWalkResponsesContent(cont, &sb)
	}
	return sb.String()
}

func pgWalkResponsesItems(items []any, sb *strings.Builder) {
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			pgWalkResponsesItem(m, sb)
		}
	}
}

// pgWalkResponsesItem extracts one output item's checkable leaves: tool
// arguments (canonicalized so \uXXXX escapes are detected decoded), message
// content text, reasoning summary text, and nested output items.
func pgWalkResponsesItem(m map[string]any, sb *strings.Builder) {
	if a, ok := m["arguments"].(string); ok && a != "" {
		sb.WriteString(pgCanonicalJSON(a))
	}
	if s, ok := m["text"].(string); ok {
		sb.WriteString(s)
	}
	if cont, ok := m["content"].([]any); ok {
		pgWalkResponsesContent(cont, sb)
	}
	if summ, ok := m["summary"].([]any); ok {
		pgWalkResponsesContent(summ, sb)
	}
	if out, ok := m["output"].([]any); ok {
		pgWalkResponsesItems(out, sb)
	}
}

func pgWalkResponsesContent(parts []any, sb *strings.Builder) {
	for _, p := range parts {
		m, _ := p.(map[string]any)
		if m == nil {
			continue
		}
		if s, ok := m["text"].(string); ok {
			sb.WriteString(s)
		}
	}
}

// pgCanonicalJSON decodes and re-encodes a JSON string value so escaped
// content (\u0043) is checked in decoded form.
func pgCanonicalJSON(raw string) string {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	if buf, err := json.Marshal(v); err == nil {
		return string(buf)
	}
	return raw
}

func (h *PlaygroundHandler) logResponsesUsage(c *gin.Context, req *PlaygroundResponsesRequest, route *router.RouteResult, providerName string, latencyMs int64, fallbackCount, retryCount int, rawResp []byte) {
	// Translate to OpenAI request shape for content logging consistency.
	oaiReq, _ := translator.ResponsesToOpenAI(&domain.ResponsesRequest{
		Model: req.Model, Input: req.Input, Instructions: req.Instructions,
	})
	respSummary := ""
	if len(rawResp) > 0 {
		respSummary = string(rawResp)
	}
	orgID := c.GetInt64("org_id")
	grTriggered, grRule, grTruncated := guardrailContext(c)
	h.logUsage("", orgID, oaiReq, 0, 0, route, providerName, latencyMs, fallbackCount, retryCount, grTriggered, grRule, grTruncated, respSummary)
}
