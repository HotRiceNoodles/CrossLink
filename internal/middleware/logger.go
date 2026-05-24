package middleware

import (
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"
)

// sanitizePath replaces numeric path segments with ":id" to avoid logging sensitive IDs.
func sanitizePath(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if _, err := strconv.Atoi(p); err == nil && len(p) > 0 {
			parts[i] = ":id"
		}
	}
	return strings.Join(parts, "/")
}

func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		latency := time.Since(start)

		requestID, _ := c.Get("request_id")

		args := []any{
			"request_id", requestID,
			"method", c.Request.Method,
			"path", sanitizePath(c.Request.URL.Path),
			"status", c.Writer.Status(),
			"latency_ms", latency.Milliseconds(),
		}

		spanCtx := trace.SpanContextFromContext(c.Request.Context())
		if spanCtx.IsValid() {
			args = append(args,
				"trace_id", spanCtx.TraceID().String(),
				"span_id", spanCtx.SpanID().String(),
			)
		}

		slog.Info("request", args...)
	}
}
