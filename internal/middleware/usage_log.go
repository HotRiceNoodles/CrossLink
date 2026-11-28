package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"gorm.io/datatypes"
)

// usageMidSem bounds the per-failed-request logging goroutines (U-24): an
// error storm (bot flood → 401/429) otherwise spawns unbounded goroutines
// each holding a 5s DB write. Overflow is counted and skipped — these rows
// are diagnostics for 0-cost failures, never billing.
var usageMidSem = make(chan struct{}, 256)

var usageMidDroppedTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "cl_usage_middleware_dropped_total",
	Help: "Failure-path usage log rows skipped because the bounded logging semaphore was full (error storms).",
})

// errBodyCaptureLimit caps the buffered error-response bytes; error bodies are
// small JSON envelopes, so 8KiB is far past any real message.
const errBodyCaptureLimit = 8 << 10

// errMessageLimit caps the error message stored in usage_logs.error_message.
const errMessageLimit = 512

func UsageLog(usageSvc *service.UsageService) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		// Buffer the error-response body (status ≥ 400 only) so gateway-side
		// rejections record the exact message the client saw — the fallback
		// row previously carried only status_code/error_type, leaving
		// "bad_request / 网关拦截" rows with no why.
		ew := &errCaptureWriter{ResponseWriter: c.Writer}
		c.Writer = ew
		c.Next()

		status := c.Writer.Status()
		if status < 400 {
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

		// Gather every context read BEFORE detaching — gin.Context is pooled
		// and reused the moment this request returns.
		entry := &service.UsageEntry{
			GinRequestID:   c.GetString("request_id"),
			RouteType:      routeType,
			ModelRequested: "-",
			ModelUsed:      "-",
			StatusCode:     status,
			ErrorType:      errorType,
			ErrorMessage:   sanitizeErrMessage(extractErrMessage(ew.buf)),
			ErrorStage:     c.GetString("error_stage"),
			LatencyMs:      time.Since(start).Milliseconds(),
			APIKeyID:       keyID,
			TeamID:         teamID,
			OrgID:          orgID,
		}
		// Guardrail context keys (set for every fired rule, blocks included):
		// the client-facing body is deliberately generic ("blocked by
		// guardrail"), so rule/reason detail travels in the usage row only.
		if v, ok := c.Get("guardrail_triggered"); ok {
			entry.GuardrailTriggered, _ = v.(bool)
			if entry.GuardrailTriggered && entry.ErrorStage == "" {
				entry.ErrorStage = "guardrail"
			}
		}
		entry.GuardrailRule = c.GetString("guardrail_rule")
		if v, ok := c.Get("security_events"); ok {
			if j, ok := v.(datatypes.JSON); ok {
				entry.SecurityEvents = j
			}
		}

		select {
		case usageMidSem <- struct{}{}:
		default:
			usageMidDroppedTotal.Inc()
			slog.Warn("usage middleware log dropped: logging semaphore full", "status", status, "error_type", errorType)
			return
		}
		go func() {
			defer func() {
				recover()
				<-usageMidSem
			}()
			logCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			usageSvc.Log(logCtx, entry)
		}()
	}
}

func logRouteTypeFromPath(pattern string) string {
	switch pattern {
	case "/v1/messages":
		return "anthropic"
	case "/v1/chat/completions":
		return "openai"
	case "/v1/responses":
		return "responses"
	case "/v1/images/generations":
		return "images"
	case "/v1/audio/speech", "/v1/audio/transcriptions", "/v1/audio/translations":
		return "audio"
	case "/v1/embeddings":
		return "embeddings"
	case "/v1/batch", "/v1/batches", "/v1/batch/:id":
		return "batch"
	case "/v1/videos", "/v1/videos/:id", "/v1/videos/:id/content":
		return "video"
	default:
		return "unknown"
	}
}

func mapStatusToErrorType(c *gin.Context, status int) string {
	// A rejection site may pin the error_type on the context (the admission
	// middleware pins invalid_request_error) — the pinned value wins over the
	// raw status bucket so the usage row says what actually rejected.
	if et := c.GetString("error_type"); et != "" {
		return et
	}
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
	case status >= 500:
		// Upstream may already have consumed tokens before a 500/502 (marshal
		// failure, unexpected response type) — record it instead of dropping.
		return "server_error"
	default:
		return "other"
	}
}

// errCaptureWriter buffers the response body of error responses (status ≥ 400)
// for the usage row's error_message. Successful and streamed responses pass
// through with a single boolean check per Write — nothing is buffered.
// WriteHeader AND WriteHeaderNow are overridden (same pattern as
// login_limiter.bodylessWriter): gin's c.JSON/AbortWithStatusJSON renders via
// c.Status + WriteHeaderNow, and the promoted interface methods would
// otherwise bypass the status observation.
type errCaptureWriter struct {
	gin.ResponseWriter
	buf     []byte
	capture bool
}

func (w *errCaptureWriter) markStatus(code int) {
	if code >= 400 {
		w.capture = true
	}
}

func (w *errCaptureWriter) WriteHeader(code int) {
	w.markStatus(code)
	w.ResponseWriter.WriteHeader(code)
}

func (w *errCaptureWriter) WriteHeaderNow() {
	w.markStatus(w.ResponseWriter.Status())
	w.ResponseWriter.WriteHeaderNow()
}

func (w *errCaptureWriter) append(b []byte) {
	if room := errBodyCaptureLimit - len(w.buf); room > 0 {
		if len(b) > room {
			b = b[:room]
		}
		w.buf = append(w.buf, b...)
	}
}

func (w *errCaptureWriter) Write(b []byte) (int, error) {
	if w.capture {
		w.append(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *errCaptureWriter) WriteString(s string) (int, error) {
	if w.capture {
		w.append([]byte(s))
	}
	return w.ResponseWriter.WriteString(s)
}

// extractErrMessage pulls the human-readable message out of a gateway error
// body. It understands the shared {"error":{"message":…}} envelope (OpenAI
// shape, guardrail, and the Anthropic wrapper which nests the same
// error.message), the plain {"error":"…"} admin shape, and falls back to the
// raw trimmed body for non-JSON text.
func extractErrMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var e struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && len(e.Error) > 0 {
		var shaped struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(e.Error, &shaped) == nil && shaped.Message != "" {
			return shaped.Message
		}
		var plain string
		if json.Unmarshal(e.Error, &plain) == nil && plain != "" {
			return plain
		}
		return ""
	}
	return strings.TrimSpace(string(body))
}

// sanitizeErrMessage keeps the recorded message single-line and bounded.
func sanitizeErrMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range msg {
		if r < 0x20 || r == 0x7f {
			b.WriteByte(' ')
		} else {
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) > errMessageLimit {
		out = out[:errMessageLimit]
		// Do not split a multi-byte rune at the cut: trim back to the last
		// valid boundary (at most a few bytes).
		for len(out) > 0 && !utf8.ValidString(out) {
			out = out[:len(out)-1]
		}
	}
	return out
}
