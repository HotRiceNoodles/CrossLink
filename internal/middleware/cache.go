package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/service"
)

const maxCacheResponseSize = 10 << 20 // 10MB max cached response

type responseCapture struct {
	gin.ResponseWriter
	body bytes.Buffer
}

func (w *responseCapture) Write(data []byte) (int, error) {
	if w.body.Len() < maxCacheResponseSize {
		w.body.Write(data)
	}
	return w.ResponseWriter.Write(data)
}

func (w *responseCapture) WriteString(s string) (int, error) {
	w.body.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

// Cache returns a middleware that caches non-streaming responses in Redis via CacheService.
// Cache hits skip rate limiting and return immediately with X-Cache: HIT header.
// Pass nil to disable caching (Community mode).
func Cache(cacheSvc service.CacheServiceInterface, cp crypto.CryptoProvider) gin.HandlerFunc {
	return func(c *gin.Context) {
		if cacheSvc == nil {
			c.Next()
			return
		}

		if !cacheSvc.IsEnabled() {
			c.Next()
			return
		}

		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}

		// Skip non-cacheable endpoints before reading body
		path := c.Request.URL.Path
		if path == "/v1/images/generations" ||
			strings.HasPrefix(path, "/v1/audio") ||
			strings.HasPrefix(path, "/v1/batch") {
			c.Next()
			return
		}

		bodyBytes := GetBodyBytes(c)
		if bodyBytes == nil {
			var err error
			bodyBytes, err = io.ReadAll(io.LimitReader(c.Request.Body, int64(cacheSvc.MaxBodySize())))
			if err != nil {
				c.Next()
				return
			}
			c.Request.Body.Close()
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		var probe struct {
			Stream bool   `json:"stream"`
			Model  string `json:"model"`
		}
		if json.Unmarshal(bodyBytes, &probe) != nil {
			c.Next()
			return
		}
		if probe.Stream {
			c.Next()
			return
		}

		ctx := c.Request.Context()

		modelTTL, disabled := cacheSvc.GetModelCacheConfig(ctx, probe.Model)
		if disabled {
			c.Header("X-Cache", "BYPASS")
			c.Next()
			return
		}

		ttl := cacheSvc.GetTTLForEndpoint(c.Request.URL.Path)
		if modelTTL > 0 {
			ttl = modelTTL
		}

		key := buildCacheKey(c.Request.URL.Path, bodyBytes, c, cp)

		cached, ok := cacheSvc.Get(ctx, key)
		if ok && cached != nil {
			c.Header("X-Cache", "HIT")
			c.Header("Content-Type", "application/json")
			c.Status(http.StatusOK)
			c.Writer.Write(cached.Body)
			c.Set("model", probe.Model)
			c.Set("provider", "cache")
			c.Set("cache_hit", true)
			if cached.InputTokens > 0 || cached.OutputTokens > 0 {
				c.Set("input_tokens", cached.InputTokens)
				c.Set("output_tokens", cached.OutputTokens)
			}
			RecordCacheHit(probe.Model)
			c.Abort()
			return
		}

		c.Header("X-Cache", "MISS")
		RecordCacheMiss(probe.Model)

		// NOTE: Thundering herd risk on cache miss — concurrent identical requests
		// will all hit the upstream provider. For this use case (LLM API proxy),
		// occasional thundering herd is acceptable because cache TTL is long (5–60 min)
		// and request volume per key is typically low. A singleflight approach was
		// considered but is complex to implement correctly with gin.Context's c.Next().

		capture := &responseCapture{ResponseWriter: c.Writer}
		c.Writer = capture
		c.Next()

		if capture.Status() == http.StatusOK && capture.body.Len() > 0 {
			if err := cacheSvc.Set(ctx, key, probe.Model, c.Request.URL.Path, capture.body.Bytes(), ttl); err != nil {
				slog.Warn("cache set failed", "error", err)
			}
		}
	}
}

// buildCacheKey generates a deterministic cache key from the request path, body, and caller identity.
// Excludes non-semantic fields: stream, stream_options, user.
// Includes api_key_id or user_id to prevent cross-user cache poisoning.
func buildCacheKey(path string, body []byte, c *gin.Context, cp crypto.CryptoProvider) string {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return "cl:cache:" + cp.HashHex(body)
	}
	delete(raw, "stream")
	delete(raw, "stream_options")
	delete(raw, "user")

	canonical, _ := json.Marshal(raw)

	// Include caller identity to isolate cache between users
	var identity string
	if keyID, exists := c.Get("api_key_id"); exists {
		if id, ok := keyID.(int64); ok {
			identity = fmt.Sprintf("key:%d", id)
		}
	}
	if identity == "" {
		if userID, exists := c.Get("user_id"); exists {
			if id, ok := userID.(int64); ok {
				identity = fmt.Sprintf("user:%d", id)
			}
		}
	}

	input := path + "\n" + identity + "\n"
	return "cl:cache:" + cp.HashHex(append([]byte(input), canonical...))
}
