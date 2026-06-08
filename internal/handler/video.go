package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
)

type VideoHandler struct {
	taskSvc       *service.VideoTaskService
	resolver      *router.Resolver
	health        *provider.HealthTracker
	usageSvc      *service.UsageService
	activeTracker service.ProviderLoadTracker
	idemCache     *service.IdempotencyCache
	budget        *provider.RetryBudget
}

func NewVideoHandler(
	taskSvc *service.VideoTaskService,
	resolver *router.Resolver,
	health *provider.HealthTracker,
	usageSvc *service.UsageService,
	activeTracker service.ProviderLoadTracker,
	idemCache *service.IdempotencyCache,
	budget *provider.RetryBudget,
) *VideoHandler {
	return &VideoHandler{
		taskSvc:       taskSvc,
		resolver:      resolver,
		health:        health,
		usageSvc:      usageSvc,
		activeTracker: activeTracker,
		idemCache:     idemCache,
		budget:        budget,
	}
}

// CreateVideo handles POST /v1/videos — submits a video generation task.
func (h *VideoHandler) CreateVideo(c *gin.Context) {
	var body []byte
	if cached := middleware.GetBodyBytes(c); cached != nil {
		body = cached
	} else {
		var err error
		body, err = io.ReadAll(io.LimitReader(c.Request.Body, maxRequestBody))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "failed to read body", "type": "invalid_request_error"}})
			return
		}
	}

	var req domain.VideoCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid json", "type": "invalid_request_error"}})
		return
	}

	if req.Prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "prompt is required", "type": "invalid_request_error"}})
		return
	}
	if req.Model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "model is required", "type": "invalid_request_error"}})
		return
	}

	c.Set("model", req.Model)

	// Extract auth context
	orgID := c.GetInt64("org_id")
	var apiKeyID int64
	if key := middleware.GetAPIKeyFromContext(c); key != nil {
		apiKeyID = key.ID
	}

	// Idempotency check
	if idemKey := c.GetHeader("X-Idempotency-Key"); idemKey != "" && h.idemCache != nil {
		if cached, ok := h.idemCache.Get(c.Request.Context(), apiKeyID, idemKey); ok {
			c.Data(cached.StatusCode, "application/json", cached.Body)
			return
		}
	}

	// Resolve routes
	routes, err := h.resolver.Resolve(c.Request.Context(), req.Model, orgID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "model not found", "type": "invalid_request_error"}})
		return
	}

	// Filter to VideoProvider routes
	var videoRoutes []*router.RouteResult
	for _, r := range routes {
		if _, ok := r.Provider.(provider.VideoProvider); ok {
			videoRoutes = append(videoRoutes, r)
		}
	}
	if len(videoRoutes) == 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
			"message": fmt.Sprintf("no video provider available for model '%s'", req.Model),
			"type":    "server_error",
			"code":    "no_video_provider",
		}})
		return
	}

	videoRoutes = service.ExpandFallbackRoutes(c.Request.Context(), h.resolver, videoRoutes, orgID)
	config := service.ResolveFallbackConfig(videoRoutes)
	engine := service.NewFallbackEngine(h.health, config)

	// Execute with fallback
	var taskResult *domain.VideoTask
	var winningRoute *router.RouteResult
	result := engine.ExecuteNonStream(c.Request.Context(), videoRoutes, func(ctx context.Context, route *router.RouteResult) (any, error) {
		vp := route.Provider.(provider.VideoProvider)
		videoReq := &domain.VideoRequest{
			Prompt:   req.Prompt,
			Model:    route.ProviderModel,
			Duration: req.Seconds,
		}
		// Convert size to aspect ratio (e.g. "1280x720" → "16:9")
		if req.Size != "" {
			videoReq.AspectRatio = sizeToAspectRatio(req.Size)
		}

		pn := route.Provider.Name()
		if h.activeTracker != nil {
			h.activeTracker.Incr(ctx, pn)
		}
		retryCfg := route.RetryConfig
		if len(videoRoutes) > 1 {
			retryCfg.NumRetries = 0
		}

		var submitErr error
		rr := provider.WithRetry(ctx, retryCfg, h.budget, func(retryCtx context.Context) error {
			taskResult, submitErr = vp.SubmitVideoTask(retryCtx, videoReq, route.ProviderRow.APIKey)
			return submitErr
		})

		if h.activeTracker != nil {
			h.activeTracker.Decr(context.Background(), pn)
		}

		if rr.Err != nil {
			return nil, rr.Err
		}
		winningRoute = route
		return taskResult, nil
	})

	if result.FinalError != nil {
		slog.Error("all video providers failed", "model", req.Model, "attempts", len(result.Attempts))
		statusCode := mapProviderErrorStatus(result.FinalError)
		c.JSON(statusCode, gin.H{"error": gin.H{"message": safeProviderError(result.FinalError), "type": "server_error", "code": "provider_error"}})
		return
	}

	task, ok := result.Response.(*domain.VideoTask)
	if !ok || task == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "unexpected response type", "type": "server_error"}})
		return
	}

	// Guard against nil winningRoute (defensive)
	if winningRoute == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "no route selected", "type": "server_error"}})
		return
	}

	inputPrice := winningRoute.InputPrice

	// Store task mapping via VideoTaskService
	gwTaskID, err := h.taskSvc.SubmitTask(c.Request.Context(), service.VideoSubmitParams{
		UpstreamTaskID: task.TaskID,
		ProviderName:   winningRoute.Provider.Name(),
		APIKey:         winningRoute.ProviderRow.APIKey,
		Model:          task.Model,
		OrgID:          orgID,
		APIKeyID:       apiKeyID,
		InputPrice:     inputPrice,
		Prompt:         truncateStr(req.Prompt, 200),
	})
	if err != nil {
		slog.Error("failed to store video task", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "failed to store task", "type": "server_error"}})
		return
	}

	// Set context values for downstream middleware
	c.Set("input_price", inputPrice)
	c.Set("provider", winningRoute.Provider.Name())
	c.Set("usage_logged", true)

	// Build OpenAI-format response
	resp := buildVideoResponse(gwTaskID, task, req)

	respBody, err := json.Marshal(resp)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "failed to marshal response", "type": "server_error"}})
		return
	}

	// Cache idempotency result
	if idemKey := c.GetHeader("X-Idempotency-Key"); idemKey != "" && h.idemCache != nil {
		h.idemCache.Set(c.Request.Context(), apiKeyID, idemKey, &service.CachedResponse{
			StatusCode: 200,
			Body:       respBody,
		})
	}

	c.Data(200, "application/json", respBody)
}

// GetVideo handles GET /v1/videos/:id — polls task status.
func (h *VideoHandler) GetVideo(c *gin.Context) {
	taskID := c.Param("id")
	orgID := c.GetInt64("org_id")

	task, state, err := h.taskSvc.GetTask(c.Request.Context(), taskID, orgID)
	if err != nil {
		handleVideoError(c, err)
		return
	}

	c.JSON(http.StatusOK, buildStatusResponse(taskID, task, state))
}

// GetVideoContent handles GET /v1/videos/:id/content — proxies video download.
func (h *VideoHandler) GetVideoContent(c *gin.Context) {
	taskID := c.Param("id")
	orgID := c.GetInt64("org_id")

	upstreamURL, err := h.taskSvc.GetContentURL(c.Request.Context(), taskID, orgID)
	if err != nil {
		handleVideoError(c, err)
		return
	}

	// Stream download with 10-minute timeout
	dlCtx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "failed to create download request", "type": "server_error"}})
		return
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if dlCtx.Err() == context.DeadlineExceeded {
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": gin.H{"message": "download timeout", "type": "server_error", "code": "download_timeout"}})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "upstream download failed", "type": "server_error"}})
		return
	}
	defer resp.Body.Close()

	// Check upstream response status — return gateway-format errors for non-200
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) // drain body
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			c.JSON(http.StatusGone, gin.H{"error": gin.H{"message": "video download link expired", "type": "invalid_request_error", "code": "video_expired"}})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "upstream download error", "type": "server_error", "code": "provider_error"}})
		return
	}

	// Size check (500MB cap)
	if resp.ContentLength > 500*1024*1024 {
		io.Copy(io.Discard, resp.Body) // drain body
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": gin.H{"message": "video file too large (max 500MB)", "type": "invalid_request_error", "code": "payload_too_large"}})
		return
	}

	// Stream response
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "video/mp4"
	}
	c.Header("Content-Type", contentType)
	if resp.ContentLength > 0 {
		c.Header("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	c.Status(http.StatusOK)
	io.Copy(c.Writer, resp.Body)
}

// handleVideoError maps service errors to HTTP responses.
func handleVideoError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrTaskNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "video task not found", "type": "invalid_request_error", "code": "video_not_found"}})
	case errors.Is(err, service.ErrTaskNotReady):
		c.JSON(http.StatusConflict, gin.H{"error": gin.H{"message": "video not ready", "type": "invalid_request_error", "code": "video_not_ready"}})
	case errors.Is(err, service.ErrTaskExpired):
		c.JSON(http.StatusGone, gin.H{"error": gin.H{"message": "video download link expired", "type": "invalid_request_error", "code": "video_expired"}})
	default:
		slog.Error("video task error", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "internal error", "type": "server_error"}})
	}
}

// buildVideoResponse constructs the initial OpenAI-format response for a created task.
func buildVideoResponse(gwTaskID string, task *domain.VideoTask, req domain.VideoCreateRequest) domain.VideoResponse {
	resp := domain.VideoResponse{
		ID:        gwTaskID,
		Object:    "video",
		Status:    mapInternalStatus(task.Status),
		Model:     task.Model,
		CreatedAt: time.Now().Unix(),
	}
	if req.Seconds > 0 {
		resp.Seconds = strconv.Itoa(req.Seconds)
	}
	if req.Size != "" {
		resp.Size = req.Size
	}
	return resp
}

// buildStatusResponse constructs a status query response.
func buildStatusResponse(gwTaskID string, task *domain.VideoTask, state *service.VideoTaskState) domain.VideoResponse {
	resp := domain.VideoResponse{
		ID:        gwTaskID,
		Object:    "video",
		Status:    mapInternalStatus(task.Status),
		Model:     state.Model,
		CreatedAt: state.CreatedAt,
	}
	if task.Status == "completed" {
		now := time.Now().Unix()
		resp.CompletedAt = &now
		resp.Output = []domain.VideoOutput{
			{Type: "url", URL: "/v1/videos/" + gwTaskID + "/content"},
		}
	}
	if task.Status == "failed" && task.Error != "" {
		resp.Error = &domain.VideoError{Message: task.Error, Code: "generation_failed"}
	}
	return resp
}

// mapInternalStatus converts internal status to OpenAI status.
func mapInternalStatus(status string) string {
	switch status {
	case "pending":
		return "queued"
	case "processing":
		return "in_progress"
	case "completed", "failed":
		return status
	default:
		return status
	}
}

// sizeToAspectRatio converts size string (e.g. "1280x720") to aspect ratio (e.g. "16:9").
func sizeToAspectRatio(size string) string {
	parts := strings.SplitN(size, "x", 2)
	if len(parts) != 2 {
		return ""
	}
	w, err1 := strconv.Atoi(parts[0])
	h, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || w == 0 || h == 0 {
		return ""
	}
	gcd := gcd(w, h)
	return fmt.Sprintf("%d:%d", w/gcd, h/gcd)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
