package translator

import (
	"strings"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/pkg/token"
)

type streamState int

const (
	stateInit        streamState = iota
	stateStarted
	stateBlockActive
	stateDone
)

type StreamTranslator struct {
	state          streamState
	messageID      string
	requestedModel string
	inputTokens    int
	outputTokens   int
	reasoningTokens int
	cacheReadTokens int
	blockIndex     int

	// Block type tracking: "text", "thinking", or "" (no active block)
	activeBlockType string

	// Tool use tracking during streaming
	activeToolID   string
	activeToolName string
	activeToolArgs strings.Builder
}

func NewStreamTranslator(messageID, requestedModel string, inputTokens int) *StreamTranslator {
	return &StreamTranslator{
		state:          stateInit,
		messageID:      messageID,
		requestedModel: requestedModel,
		inputTokens:    inputTokens,
	}
}

func (t *StreamTranslator) InputTokens() int     { return t.inputTokens }
func (t *StreamTranslator) OutputTokens() int    { return t.outputTokens }
func (t *StreamTranslator) ReasoningTokens() int { return t.reasoningTokens }
func (t *StreamTranslator) CacheReadTokens() int { return t.cacheReadTokens }

func GenerateMessageID() string { return generateMessageID() }

func (t *StreamTranslator) TranslateChunk(sseChunk domain.SSEChunk) []domain.SSEEvent {
	if sseChunk.Done {
		return nil
	}

	chunk := sseChunk.Chunk
	if chunk == nil {
		return nil
	}

	var events []domain.SSEEvent

	if t.state == stateInit {
		if chunk.Usage != nil && chunk.Usage.PromptTokens > 0 {
			t.inputTokens = chunk.Usage.PromptTokens
		}
		events = append(events, buildMessageStart(t.messageID, t.requestedModel, t.inputTokens))
		t.state = stateStarted
	}

	if len(chunk.Choices) == 0 {
		if chunk.Usage != nil {
			t.inputTokens = chunk.Usage.PromptTokens
			t.outputTokens = chunk.Usage.CompletionTokens
			if chunk.Usage.CompletionTokensDetails != nil {
				t.reasoningTokens = chunk.Usage.CompletionTokensDetails.ReasoningTokens
			}
			if chunk.Usage.PromptTokensDetails != nil {
				t.cacheReadTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			}
		}
		return events
	}

	delta := chunk.Choices[0]

	// Handle reasoning content (thinking blocks)
	if delta.Delta.ReasoningContent != "" {
		if t.state == stateStarted {
			events = append(events, thinkingBlockStart(t.blockIndex))
			t.activeBlockType = "thinking"
			t.state = stateBlockActive
		}
		if t.state == stateBlockActive && t.activeBlockType == "thinking" {
			events = append(events, thinkingBlockDelta(t.blockIndex, delta.Delta.ReasoningContent))
			t.outputTokens += token.Estimate(delta.Delta.ReasoningContent)
		}
	}

	// Handle tool_calls in delta
	if len(delta.Delta.ToolCalls) > 0 {
		events = append(events, t.translateToolCallDelta(delta.Delta.ToolCalls)...)
	}

	// Handle text content
	if delta.Delta.Content != "" {
		// Close thinking block if active, transition to text
		if t.state == stateBlockActive && t.activeBlockType == "thinking" {
			events = append(events, contentBlockStop(t.blockIndex))
			t.blockIndex++
			t.activeBlockType = ""
			t.state = stateStarted
		}
		if t.state == stateStarted {
			events = append(events, contentBlockStart(t.blockIndex))
			t.activeBlockType = "text"
			t.state = stateBlockActive
		}
		if t.state == stateBlockActive && t.activeBlockType == "text" {
			events = append(events, contentBlockDelta(t.blockIndex, delta.Delta.Content))
			t.outputTokens += token.Estimate(delta.Delta.Content)
		}
	}

	if delta.FinishReason != nil && *delta.FinishReason != "" {
		// Close any active tool_use block
		toolEvents := t.closeActiveToolBlock()
		events = append(events, toolEvents...)
		// If still blockActive, close the thinking or text block.
		if t.state == stateBlockActive {
			events = append(events, contentBlockStop(t.blockIndex))
			t.activeBlockType = ""
		}

		if chunk.Usage != nil {
			if chunk.Usage.PromptTokens > 0 {
				t.inputTokens = chunk.Usage.PromptTokens
			}
			if chunk.Usage.CompletionTokens > 0 {
				t.outputTokens = chunk.Usage.CompletionTokens
			}
			if chunk.Usage.CompletionTokensDetails != nil {
				t.reasoningTokens = chunk.Usage.CompletionTokensDetails.ReasoningTokens
			}
			if chunk.Usage.PromptTokensDetails != nil {
				t.cacheReadTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			}
		}

		stopReason := finishReasonToStopReason(*delta.FinishReason)
		events = append(events, messageDelta(stopReason, t.outputTokens))
		events = append(events, messageStop())
		t.state = stateDone
	}

	return events
}

func (t *StreamTranslator) translateToolCallDelta(toolCalls []domain.OpenAIChunkToolCall) []domain.SSEEvent {
	var events []domain.SSEEvent

	for _, tc := range toolCalls {
		// New tool call starts when ID is present
		if tc.ID != "" {
			// Close any active content block (thinking, text, or tool_use)
			if t.state == stateBlockActive {
				events = append(events, t.closeActiveToolBlock()...)
				if t.state == stateBlockActive {
					events = append(events, contentBlockStop(t.blockIndex))
					t.blockIndex++
					t.activeBlockType = ""
					t.state = stateStarted
				}
			}

			// Start a new tool_use content block
			events = append(events, toolUseBlockStart(t.blockIndex, tc.ID, tc.Function.Name))
			t.activeToolID = tc.ID
			t.activeToolName = tc.Function.Name
			t.activeToolArgs.Reset()
			t.state = stateBlockActive
		}

		// Arguments delta
		if tc.Function.Arguments != "" {
			if t.activeToolID != "" {
				events = append(events, toolUseArgumentsDelta(t.blockIndex, tc.Function.Arguments))
				if t.activeToolArgs.Len() > 1<<20 { // 1MB max
					continue
				}
				t.activeToolArgs.WriteString(tc.Function.Arguments)
			}
		}
	}

	return events
}

func (t *StreamTranslator) closeActiveToolBlock() []domain.SSEEvent {
	if t.activeToolID == "" {
		return nil
	}
	events := []domain.SSEEvent{contentBlockStop(t.blockIndex)}
	t.blockIndex++
	t.activeToolID = ""
	t.activeToolName = ""
	t.activeToolArgs.Reset()
	t.state = stateStarted
	return events
}

func (t *StreamTranslator) OnProviderDisconnect() []domain.SSEEvent {
	var events []domain.SSEEvent

	switch t.state {
	case stateBlockActive:
		if t.activeToolID != "" {
			events = append(events, t.closeActiveToolBlock()...)
		} else {
			events = append(events, contentBlockStop(t.blockIndex))
			t.activeBlockType = ""
		}
		fallthrough
	case stateStarted:
		events = append(events, messageDelta("end_turn", t.outputTokens))
		events = append(events, messageStop())
	case stateInit:
		events = append(events, buildMessageStart(t.messageID, t.requestedModel, 0))
		events = append(events, messageDelta("end_turn", 0))
		events = append(events, messageStop())
	}

	t.state = stateDone
	return events
}
