package middleware

import (
	"net/http"
	"strings"

	"github.com/crosslink/internal/admission"
	"github.com/crosslink/internal/domain"
	"github.com/gin-gonic/gin"
)

// AdmissionEnvelopeKey is the gin context key under which the pre-cache
// admission middleware publishes the admitted ProtocolExtensions envelope.
// Handlers consume it instead of re-deriving from the raw body; the key is
// always SET when the middleware ran (value may be a nil *ProtocolExtensions,
// meaning "admitted, nothing to carry").
const AdmissionEnvelopeKey = "crosslink_admission_envelope"

// Admission is the M3.4 pre-cache admission gate (design §2): it runs the
// parameter/beta-header classification BEFORE the response cache and every
// other replayable early-return, so a cached response can never bypass the
// current rules. Scope: exactly /v1/messages and /v1/chat/completions —
// /v1/responses is already excluded from the cache, and other endpoints
// (videos, audio, …) have their own parameter surfaces that would false-trip
// the OpenAI/Anthropic chat classification.
//
// Fast path: no beta header + POST body from the ReadBodyWithBudget cache;
// the classification is one map unmarshal — no candidate resolution happens
// here (that binding arrives with per-candidate application).
func Admission() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if c.Request.Method != http.MethodPost ||
			(path != "/v1/messages" && path != "/v1/chat/completions") {
			c.Next()
			return
		}

		body := GetBodyBytes(c)
		if body == nil {
			// No cached body (read failure upstream): let the handler's own
			// admission fallback decide — never skip the rules silently.
			c.Next()
			return
		}

		var beta string
		if strings.HasSuffix(path, "/v1/messages") {
			beta = c.GetHeader("anthropic-beta")
		}

		env, err := admission.Admit(sourceForPath(path), body, beta)
		if err != nil {
			// Rejections are client errors with a stable message; the UsageLog
			// middleware (earlier in the chain) records the 400 row, pinned to
			// invalid_request_error + the admission stage so parameter-rule
			// rejections are distinguishable from other 400s in usage logs.
			c.Set("error_type", "invalid_request_error")
			c.Set("error_stage", "admission")
			if strings.HasSuffix(path, "/v1/messages") {
				c.JSON(http.StatusBadRequest, gin.H{
					"type":  "error",
					"error": gin.H{"type": "invalid_request_error", "message": err.Error()},
				})
			} else {
				c.JSON(http.StatusBadRequest, gin.H{"error": map[string]string{"message": err.Error()}})
			}
			c.Abort()
			return
		}

		var envPtr *domain.ProtocolExtensions = env
		c.Set(AdmissionEnvelopeKey, envPtr)
		c.Next()
	}
}

func sourceForPath(path string) string {
	if strings.HasSuffix(path, "/v1/messages") {
		return "anthropic"
	}
	return "openai"
}
