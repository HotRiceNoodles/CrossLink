package translator

import (
	"encoding/json"
	"testing"

	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/assert"
)

func TestStreamTranslator_ReasoningTokens(t *testing.T) {
	st := NewStreamTranslator("msg_test", "test-model", 0)

	// Start stream
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`))

	// Usage-only chunk with reasoning tokens
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":20,"total_tokens":70,"completion_tokens_details":{"reasoning_tokens":8}}}`))

	assert.Equal(t, 8, st.ReasoningTokens())
	assert.Equal(t, 0, st.CacheReadTokens())
}

func TestStreamTranslator_CacheReadTokens(t *testing.T) {
	st := NewStreamTranslator("msg_test", "test-model", 0)

	// Start stream
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`))

	// Usage-only chunk with cache read tokens
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":30,"total_tokens":130,"prompt_tokens_details":{"cached_tokens":45}}}`))

	assert.Equal(t, 45, st.CacheReadTokens())
	assert.Equal(t, 0, st.ReasoningTokens())
}

func TestStreamTranslator_BothTokenDetails(t *testing.T) {
	st := NewStreamTranslator("msg_test", "test-model", 0)

	// Start stream
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`))

	// Usage-only chunk with both
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":30,"total_tokens":130,"completion_tokens_details":{"reasoning_tokens":12},"prompt_tokens_details":{"cached_tokens":60}}}`))

	assert.Equal(t, 12, st.ReasoningTokens())
	assert.Equal(t, 60, st.CacheReadTokens())
}

func TestStreamTranslator_FinishReasonTokenDetails(t *testing.T) {
	st := NewStreamTranslator("msg_test", "test-model", 0)

	// Start + content
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`))

	// Finish with usage details
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60,"completion_tokens_details":{"reasoning_tokens":3},"prompt_tokens_details":{"cached_tokens":20}}}`))

	assert.Equal(t, 3, st.ReasoningTokens())
	assert.Equal(t, 20, st.CacheReadTokens())
}

func TestStreamTranslator_NilTokenDetails(t *testing.T) {
	st := NewStreamTranslator("msg_test", "test-model", 0)

	// Start + content
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`))

	// Usage without details
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60}}`))

	assert.Equal(t, 0, st.ReasoningTokens())
	assert.Equal(t, 0, st.CacheReadTokens())
}

// Verify the chunk helper parses PromptTokensDetails correctly
func TestOpenAIChunkUsage_PromptTokensDetails_Parse(t *testing.T) {
	data := `{"prompt_tokens":100,"completion_tokens":30,"total_tokens":130,"prompt_tokens_details":{"cached_tokens":45}}`
	var usage domain.OpenAIChunkUsage
	err := json.Unmarshal([]byte(data), &usage)
	assert.NoError(t, err)
	assert.NotNil(t, usage.PromptTokensDetails)
	assert.Equal(t, 45, usage.PromptTokensDetails.CachedTokens)
}

func TestOpenAIChunkUsage_CompletionTokensDetails_Parse(t *testing.T) {
	data := `{"prompt_tokens":100,"completion_tokens":30,"total_tokens":130,"completion_tokens_details":{"reasoning_tokens":8}}`
	var usage domain.OpenAIChunkUsage
	err := json.Unmarshal([]byte(data), &usage)
	assert.NoError(t, err)
	assert.NotNil(t, usage.CompletionTokensDetails)
	assert.Equal(t, 8, usage.CompletionTokensDetails.ReasoningTokens)
}