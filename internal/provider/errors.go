package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

type ErrorType string

const (
	ErrorRateLimit  ErrorType = "rate_limit"
	ErrorAuth       ErrorType = "auth"
	ErrorNotFound   ErrorType = "not_found"
	ErrorBadRequest ErrorType = "bad_request"
	ErrorServer     ErrorType = "server"
	ErrorNetwork    ErrorType = "network"
	ErrorTimeout    ErrorType = "timeout"
	ErrorQuota      ErrorType = "quota"
)

// ProviderError carries the upstream HTTP status code and error type.
type ProviderError struct {
	StatusCode int
	Message    string
	ErrorType  ErrorType
	Code       string
	Type       string
	RetryAfter time.Duration
}

func (e *ProviderError) Error() string {
	return e.Message
}

func parseProviderError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap

	var errResp struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &errResp)

	msg := errResp.Error.Message
	if msg == "" {
		msg = string(body)
	}

	var label string
	var et ErrorType
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		label = "provider auth failed"
		et = ErrorAuth
	case http.StatusTooManyRequests:
		label = "provider rate limited"
		et = ErrorRateLimit
	case http.StatusBadRequest:
		label = "provider bad request"
		et = ErrorBadRequest
	case http.StatusNotFound:
		label = "provider not found"
		et = ErrorNotFound
	default:
		label = fmt.Sprintf("provider error (%d)", resp.StatusCode)
		et = ErrorServer
	}

	pe := &ProviderError{
		StatusCode: resp.StatusCode,
		Message:    fmt.Sprintf("%s: %s", label, msg),
		ErrorType:  et,
		Code:       errResp.Error.Code,
		Type:       errResp.Error.Type,
	}

	// Parse Retry-After header (rate-limit, 503, and other retryable errors)
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
			if secs > 300 {
				secs = 300
			}
			pe.RetryAfter = time.Duration(secs) * time.Second
		}
	}

	return pe
}

func ClassifyError(err error) ErrorType {
	if err == nil {
		return ""
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.ErrorType
	}
	// Client-initiated cancellations are not provider errors
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ""
	}
	// Distinguish timeout from other network errors
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return ErrorTimeout
		}
		return ErrorNetwork
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrorNetwork
	}
	return "" // unknown errors are not retryable
}

func IsRetryableError(err error) bool {
	switch ClassifyError(err) {
	case ErrorAuth, ErrorNotFound, ErrorBadRequest:
		return false
	case "":
		return false // context.Canceled or nil
	default:
		return true
	}
}
