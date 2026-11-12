package debug

import (
	"bytes"
	crand "crypto/rand"
	"encoding/json"
	"io"
	"time"

	"github.com/crosslink/internal/debug/upstream"
	"github.com/crosslink/internal/middleware"
	"github.com/gin-gonic/gin"
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

		// 采样策略（管理端运行时策略，调试数据安全）：onlyFailed 只留失败请求；
		// samplingRate 按百分比随机丢弃。均为记录前最后判定，不影响请求本身。
		if store.OnlyFailed() && entry.RespStatus < 400 {
			return
		}
		if rate := store.SamplingRate(); rate < 100 {
			if rate <= 0 {
				return
			}
			if !sampleHit(rate) {
				return
			}
		}
		store.Add(entry)
	}
}

// sampleHit reports whether this request falls inside a rate% sample window.
// Uses crypto/rand so an observer cannot predict which requests are recorded.
func sampleHit(rate int) bool {
	var b [1]byte
	if _, err := crand.Read(b[:]); err != nil {
		return true // 读取失败宁可多记（诊断可用性优先），不静默丢
	}
	return int(b[0]) < rate*255/100
}
