package upstream

import (
	"net/http"
	"time"
)

// Body size limits for upstream capture.
const (
	UpstreamBodyLimit  = 256 * 1024      // 256KB — single body per upstream call
	UpstreamTotalLimit = 2 * 1024 * 1024  // 2MB — total upstream data per Entry
	StreamBodyLimit    = 64 * 1024       // 64KB — SSE stream capture cap
)

// UpstreamCall records a single gateway→upstream provider HTTP call.
type UpstreamCall struct {
	Provider string // "openai_compatible", "anthropic", "azure_openai", etc.
	Model    string // actual model name sent to upstream
	BaseURL  string // upstream endpoint
	Method   string // HTTP method
	Path     string // request path

	// Request info
	ReqHeaders http.Header // redacted headers
	ReqBody    []byte      // truncated to UpstreamBodyLimit

	// Response info
	StatusCode  int         // upstream HTTP status code
	RespHeaders http.Header // redacted headers
	RespBody    []byte      // non-stream: truncated; stream: first 64KB

	// Metadata
	Duration   time.Duration // call duration
	Attempt    int           // attempt number (1-based)
	IsRetry    bool          // true if Attempt > 1
	IsFallback bool          // true if this is a fallback route
	Error      string        // error message if call failed
}
