package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"

	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
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

// providerErrorDetail extracts the sanitized error text plus the upstream
// status/code for usage-log persistence (error observability L1). For
// non-provider errors (gateway-side rejections) upstreamStatus is 0, which is
// stored as NULL to distinguish them from upstream rejections.
func providerErrorDetail(err error) (msg string, upstreamStatus int, upstreamCode string) {
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		return sanitizeProviderMessage(pe.Message), pe.StatusCode, pe.Code
	}
	return sanitizeProviderMessage(err.Error()), 0, ""
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
// common sensitive patterns like org-xxx account identifiers.
func sanitizeProviderMessage(msg string) string {
	if len(msg) > 200 {
		msg = msg[:200]
	}
	// Strip common account/org ID patterns (e.g. org-abc123, org_abc123)
	msg = regexpOrgID.ReplaceAllString(msg, "[REDACTED]")
	return msg
}

var regexpOrgID = regexp.MustCompile(`(?i)\borg[_-][a-zA-Z0-9]{4,}\b`)
