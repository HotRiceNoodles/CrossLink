package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"

	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	"github.com/crosslink/internal/translator"
)

// resolveErrorStatus maps a Resolver error to an HTTP status. Alias-on-Community
// is 403; everything else stays 404 (the historical behavior).
func resolveErrorStatus(err error) int {
	if errors.Is(err, router.ErrProRequired) {
		return http.StatusForbidden
	}
	return http.StatusNotFound
}

// mapProviderErrorStatus maps a provider error to an appropriate HTTP status code.
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

// providerRateLimited reports whether err is an upstream 429.
func providerRateLimited(err error) bool {
	var pe *provider.ProviderError
	return errors.As(err, &pe) && pe.StatusCode == http.StatusTooManyRequests
}

// providerRetryAfterHeader sets the Retry-After response header when err carries
// an upstream hint, so clients can back off correctly on a propagated 429.
func providerRetryAfterHeader(c *gin.Context, err error) {
	var pe *provider.ProviderError
	if errors.As(err, &pe) && pe.RetryAfter > 0 {
		c.Header("Retry-After", fmt.Sprintf("%d", int(pe.RetryAfter.Seconds())))
	}
}

// safeProviderError returns a client-safe error message.
// It preserves provider error messages (already user-facing) but replaces
// internal errors with a generic message to avoid leaking infrastructure details.
// Provider messages are truncated and stripped of common sensitive patterns
// (account IDs, organization IDs) to avoid leaking credentials.
func safeProviderError(err error) string {
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		return sanitizeProviderMessage(pe.Message)
	}
	return "upstream provider error"
}

// errorDetail is everything the usage log needs to diagnose one failure
// (error observability L1.5).
type errorDetail struct {
	Message        string // sanitized, storage-width (1000 runes)
	UpstreamStatus int    // 0 = gateway-side rejection, stored as NULL
	Code           string // upstream error.code
	Type           string // upstream error.type (e.g. invalid_request_error)
	Param          string // upstream error.param — the rejected request field
	Stage          string // request | resolve | translate | upstream | internal
}

// errorInfo classifies a gateway error for the usage log. Stage says WHERE the
// request died; the upstream fields say what the provider said when it did.
func errorInfo(err error) errorDetail {
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		return errorDetail{
			Message:        sanitizeMessageForLog(pe.Message),
			UpstreamStatus: pe.StatusCode,
			Code:           pe.Code,
			Type:           pe.Type,
			Param:          pe.Param,
			Stage:          "upstream",
		}
	}
	stage := "internal"
	switch {
	case errors.Is(err, translator.ErrMissingModel),
		errors.Is(err, translator.ErrMissingMessages),
		errors.Is(err, translator.ErrMissingMaxTokens):
		stage = "translate"
	case errors.Is(err, router.ErrProRequired):
		stage = "resolve"
	}
	return errorDetail{Message: sanitizeMessageForLog(err.Error()), Stage: stage}
}

// attemptRecord is the persisted shape of one fallback attempt. The error
// message is deliberately not included — only the FinalError's message is
// stored (L1 error_message) to avoid duplicating text per row.
type attemptRecord struct {
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	ErrorType      string `json:"error_type,omitempty"`
	UpstreamStatus int    `json:"upstream_status,omitempty"`
	LatencyMs      int64  `json:"latency_ms"`
	Success        bool   `json:"success"`
	Persistent     bool   `json:"persistent,omitempty"`
}

// attemptsJSON serializes the fallback timeline for the usage log (error
// observability L2). Clean single-attempt successes return nil so the column
// stays NULL and row size stays flat for the common case.
func attemptsJSON(attempts []service.FallbackAttempt) datatypes.JSON {
	if len(attempts) == 0 || (len(attempts) == 1 && attempts[0].Success) {
		return nil
	}
	records := make([]attemptRecord, 0, len(attempts))
	for _, a := range attempts {
		records = append(records, attemptRecord{
			Provider:       a.ProviderName,
			Model:          a.ProviderModel,
			ErrorType:      string(a.ErrorType),
			UpstreamStatus: a.UpstreamStatus,
			LatencyMs:      a.LatencyMs,
			Success:        a.Success,
			Persistent:     a.Persistent,
		})
	}
	b, err := json.Marshal(records)
	if err != nil {
		return nil
	}
	return datatypes.JSON(b)
}

// sanitizeProviderMessage truncates provider error messages and strips
// common sensitive patterns like org-xxx account identifiers. Client-facing
// width: 200.
func sanitizeProviderMessage(msg string) string {
	return sanitizeMessage(msg, clientMessageLimit)
}

// sanitizeMessageForLog is the storage-width variant: the usage-log copy is
// admin-only, so it keeps up to 1000 runes (error observability L1.5 — the
// locator details often sit at the end of long upstream JSON errors).
func sanitizeMessageForLog(msg string) string {
	return sanitizeMessage(msg, storageMessageLimit)
}

const (
	clientMessageLimit  = 200
	storageMessageLimit = 1000
)

// sanitizeMessage truncates at a rune boundary (a plain byte slice would cut
// multi-byte CJK characters in half and leave mojibake) and strips common
// account/org ID patterns (e.g. org-abc123, org_abc123).
func sanitizeMessage(msg string, limit int) string {
	if runes := []rune(msg); len(runes) > limit {
		msg = string(runes[:limit])
	}
	// Strip common account/org ID patterns (e.g. org-abc123, org_abc123)
	msg = regexpOrgID.ReplaceAllString(msg, "[REDACTED]")
	return msg
}

var regexpOrgID = regexp.MustCompile(`(?i)\borg[_-][a-zA-Z0-9]{4,}\b`)

// logRequestStageError records a usage row for rejections that return before
// routing (parse / modality / resolve). These previously left no trace in the
// logs at all — a blind spot when counting 4xx (error observability L1.5).
func logRequestStageError(usageSvc *service.UsageService, c *gin.Context, routeType, model string, statusCode int, errorType, stage, message string) {
	if usageSvc == nil {
		return
	}
	start := time.Now()
	var keyID int64
	var teamID int64
	orgID := c.GetInt64("org_id")
	if key := middleware.GetAPIKeyFromContext(c); key != nil {
		keyID = key.ID
		if key.TeamID != nil {
			teamID = *key.TeamID
		}
	}
	c.Set("usage_logged", true)
	submitUsage(func() {
		usageSvc.Log(context.Background(), &service.UsageEntry{
			RouteType:      routeType,
			ModelRequested: model,
			APIKeyID:       keyID,
			TeamID:         teamID,
			OrgID:          orgID,
			StatusCode:     statusCode,
			ErrorType:      errorType,
			ErrorMessage:   sanitizeMessageForLog(message),
			ErrorStage:     stage,
			LatencyMs:      time.Since(start).Milliseconds(),
		})
	})
}
