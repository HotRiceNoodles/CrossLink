package handler

import (
	"github.com/crosslink/internal/middleware"
	"github.com/gin-gonic/gin"
)

// readTemplateID reads the prompt-template id set by ContextAssembler on the
// gin context, if any. Must be called SYNCHRONOUSLY in the handler (not inside
// an async submitUsage closure) because gin may recycle the context after the
// response is sent. Returns nil when no template was used.
func readTemplateID(c *gin.Context) *int64 {
	v, ok := c.Get("template_id")
	if !ok {
		return nil
	}
	id, ok := v.(int64)
	if !ok || id == 0 {
		return nil
	}
	return &id
}

// readPriceMultiplier reads the API key's price multiplier from the gin context.
// Same synchronous-read rule as readTemplateID. Defaults to 1.0 when no key or
// key has no multiplier set.
func readPriceMultiplier(c *gin.Context) float64 {
	key := middleware.GetAPIKeyFromContext(c)
	if key == nil || key.PriceMultiplier <= 0 {
		return 1.0
	}
	return key.PriceMultiplier
}
