package handler

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/crosslink/internal/provider"
)

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
