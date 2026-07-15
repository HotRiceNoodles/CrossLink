package handler

import "github.com/gin-gonic/gin"

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
