package middleware

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"
)

// Tracing returns a Gin middleware that creates an OpenTelemetry span for each request.
// It extracts trace context from incoming headers, creates a server-span, and records
// HTTP semantic attributes. Downstream handlers can add "model" and "provider" to the
// Gin context to have them attached to the span.
func Tracing() gin.HandlerFunc {
	return func(c *gin.Context) {
		propagator := otel.GetTextMapPropagator()
		ctx := propagator.Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))

		tracer := otel.Tracer("github.com/crosslink/middleware")
		spanName := c.Request.Method + " " + c.FullPath()
		if spanName == "GET " || spanName == "POST " {
			spanName = c.Request.Method + " /unknown"
		}

		ctx, span := tracer.Start(ctx, spanName,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				semconv.HTTPRequestMethodKey.String(c.Request.Method),
				semconv.URLPathKey.String(c.Request.URL.Path),
				semconv.HTTPRequestBodySize(int(c.Request.ContentLength)),
			),
		)
		defer span.End()

		// Store span in context
		c.Request = c.Request.WithContext(ctx)

		// Set request_id as span attribute if available
		if reqID := c.GetString("request_id"); reqID != "" {
			span.SetAttributes(attribute.String("request_id", reqID))
		}

		c.Next()

		// Record response status
		status := c.Writer.Status()
		span.SetAttributes(semconv.HTTPResponseStatusCodeKey.Int(status))
		if status >= 400 {
			span.SetStatus(codes.Error, http.StatusText(status))
		} else {
			span.SetStatus(codes.Ok, "")
		}

		// Add model and provider if set by downstream handlers
		if model, exists := c.Get("model"); exists {
			span.SetAttributes(attribute.String("llm.model", fmt.Sprintf("%v", model)))
		}
		if provider, exists := c.Get("provider"); exists {
			span.SetAttributes(attribute.String("llm.provider", fmt.Sprintf("%v", provider)))
		}
	}
}
