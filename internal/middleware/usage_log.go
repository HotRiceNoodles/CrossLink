package middleware

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/service"
)

func UsageLog(usageSvc *service.UsageService) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		status := c.Writer.Status()
		if status < 400 {
			return
		}
		if status >= 500 && status != 503 {
			return
		}

		if _, logged := c.Get("usage_logged"); logged {
			return
		}

		var keyID, teamID int64
		orgID := c.GetInt64("org_id")
		if key := GetAPIKeyFromContext(c); key != nil {
			keyID = key.ID
			if key.TeamID != nil {
				teamID = *key.TeamID
			}
		}
		routeType := logRouteTypeFromPath(c.FullPath())
		errorType := mapStatusToErrorType(c, status)

		if !usageSvc.IsMiddlewareLogEnabled(errorType) {
			return
		}

		go func() {
			defer func() { recover() }()
			logCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			usageSvc.Log(logCtx, &service.UsageEntry{
				RouteType:      routeType,
				ModelRequested: "-",
				ModelUsed:      "-",
				Currency:       "CNY",
				StatusCode:     status,
				ErrorType:      errorType,
				LatencyMs:      time.Since(start).Milliseconds(),
				APIKeyID:       keyID,
				TeamID:         teamID,
			OrgID:          orgID,
			})
		}()
	}
}

func logRouteTypeFromPath(pattern string) string {
	switch pattern {
	case "/v1/messages":
		return "anthropic"
	case "/v1/chat/completions":
		return "openai"
	default:
		return "unknown"
	}
}

func mapStatusToErrorType(c *gin.Context, status int) string {
	if status == 429 {
		if _, ok := c.Get("budget_exceeded"); ok {
			return "budget_exceeded"
		}
		if _, ok := c.Get("call_limit_exceeded"); ok {
			return "call_limit_exceeded"
		}
		return "rate_limit"
	}
	switch {
	case status == 400:
		return "bad_request"
	case status == 401:
		return "auth_failure"
	case status == 403:
		return "forbidden"
	case status == 404:
		return "not_found"
	case status == 503:
		return "service_unavailable"
	default:
		return "other"
	}
}
