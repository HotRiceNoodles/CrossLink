package debug

import (
	"bytes"
	"encoding/json"
	"io"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/debug/upstream"
	"github.com/crosslink/internal/middleware"
)

// extractModel reads the "model" field from a JSON request body.
func extractModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &v); err == nil && v.Model != "" {
		return v.Model
	}
	return ""
}

func Middleware(store *Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !store.IsEnabled() {
			c.Next()
			return
		}

		// Capture request headers (redact sensitive ones)
		reqHeaders := upstream.RedactHeaders(c.Request.Header)

		// Read full request body — prefer cached bytes from ReadBody middleware
		var fullBody []byte
		if cached := middleware.GetBodyBytes(c); cached != nil {
			fullBody = cached
		} else {
			maxRead := int64(10 * 1024 * 1024)
			fullBody, _ = io.ReadAll(io.LimitReader(c.Request.Body, maxRead))
			c.Request.Body.Close()
			c.Request.Body = io.NopCloser(bytes.NewReader(fullBody))
		}

		// Truncate for storage
		storeBody := fullBody
		reqTruncated := len(storeBody) > store.MaxBodySize()
		if reqTruncated {
			storeBody = storeBody[:store.MaxBodySize()]
		}

		// Wrap response writer
		wrapper := newResponseCaptureWriter(c.Writer, store.MaxBodySize())
		c.Writer = wrapper

		// Inject upstream collector into context for debugTransport
		collector := &upstream.UpstreamCollector{}
		c.Request = c.Request.WithContext(upstream.WithUpstreamCollector(c.Request.Context(), collector))

		start := time.Now()
		c.Next()
		elapsed := time.Since(start)

		// Detect streaming
		stream := wrapper.Header().Get("Content-Type") == "text/event-stream"

		entry := &Entry{
			ID:           c.GetString("request_id"),
			Timestamp:    time.Now(),
			Duration:     elapsed,
			Method:       c.Request.Method,
			Path:         c.Request.URL.Path,
			Model:        extractModel(fullBody),
			Stream:       stream,
			Truncated:    reqTruncated || wrapper.IsTruncated(),
			ReqHeaders:   reqHeaders,
			ReqBody:      storeBody,
			RespStatus:   wrapper.Status(),
			RespHeaders:  upstream.RedactHeaders(wrapper.Header()),
			RespBody:     wrapper.CapturedBody(),
			InputTokens:  c.GetInt("input_tokens"),
			OutputTokens: c.GetInt("output_tokens"),
		}
		entry.UpstreamCalls = collector.Calls()
		orgID, _ := c.Get("org_id")
		entry.OrgID, _ = orgID.(int64)
		store.Add(entry)
	}
}
