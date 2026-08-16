package handler

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"

	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
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
