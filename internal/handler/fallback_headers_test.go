package handler

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestSetFallbackHeaders(t *testing.T) {
	// Fallback occurred: both headers set.
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	setFallbackHeaders(c, "gpt-fallback", 2)
	assert.Equal(t, "gpt-fallback", c.Writer.Header().Get("x-crosslink-fallback-model"))
	assert.Equal(t, "2", c.Writer.Header().Get("x-crosslink-fallback-count"))

	// No fallback: model still reported, count omitted.
	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	setFallbackHeaders(c2, "gpt-fallback", 0)
	assert.Equal(t, "gpt-fallback", c2.Writer.Header().Get("x-crosslink-fallback-model"))
	assert.Empty(t, c2.Writer.Header().Get("x-crosslink-fallback-count"))

	// Empty model: header omitted (nil-safe).
	c3, _ := gin.CreateTestContext(httptest.NewRecorder())
	setFallbackHeaders(c3, "", 1)
	assert.Empty(t, c3.Writer.Header().Get("x-crosslink-fallback-model"))
	assert.Equal(t, "1", c3.Writer.Header().Get("x-crosslink-fallback-count"))
}

func TestWriteStreamInterrupted(t *testing.T) {
	var buf bytes.Buffer
	writeStreamInterrupted(&buf)
	out := buf.String()
	assert.True(t, strings.HasPrefix(out, "data: "), "must be an OpenAI SSE data line: %q", out)
	assert.Contains(t, out, `"type":"stream_interrupted"`)
	assert.Contains(t, out, "upstream stream ended unexpectedly")
	assert.Contains(t, out, "\n\n", "must terminate the SSE event with a blank line")
}

func TestWriteStreamInterruptedAnthropic(t *testing.T) {
	var buf bytes.Buffer
	writeStreamInterruptedAnthropic(&buf)
	out := buf.String()
	// Anthropic SSE uses named events; the interrupt is an error event followed by
	// the terminal message_stop so clients see a clean (if abrupt) end.
	assert.Contains(t, out, "event: error\n")
	assert.Contains(t, out, `"type":"stream_interrupted"`)
	assert.Contains(t, out, "event: message_stop\n")
	assert.Contains(t, out, `{"type":"message_stop"}`)
}
