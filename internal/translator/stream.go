package translator

import (
	"strings"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/pkg/token"
)

type streamState int

const (
	stateInit streamState = iota
	stateStarted
	stateBlockActive
	stateDone
)

type StreamTranslator struct {
	state               streamState
	messageID           string
	requestedModel      string
	inputTokens         int
	outputTokens        int
	usageSeen           bool // upstream sent a real input usage number (not the seed estimate)
	reasoningTokens     int
	cacheReadTokens     int
	cacheCreationTokens int
	blockIndex          int

	// Block type tracking: "text", "thinking", or "" (no active block)
	activeBlockType string

	// Tool use tracking during streaming, keyed by the OpenAI tool_calls index.
	tools         map[int]*toolStreamState
	toolOrder     []int // first-seen order, used to flush blocks deterministically
	activeToolIdx int   // OpenAI index of the tool whose block is open; -1 = none
}

// toolStreamState tracks one in-flight tool call. Anthropic content blocks are
// strictly sequential (start → deltas → stop, never reopened), while OpenAI may
// interleave argument fragments across parallel tool calls (all headers first,
// then fragments by index). So only the currently open tool block streams its
// deltas inline; fragments for other tools buffer here and flush in order when
// the earlier blocks can be closed (at finish / provider disconnect).
type toolStreamState struct {
	id   string
	name string
	args strings.Builder

	blockIndex int // assigned when the block is opened
	opened     bool
	closed     bool
}

func NewStreamTranslator(messageID, requestedModel string, inputTokens int) *StreamTranslator {
	return &StreamTranslator{
		state:          stateInit,
		messageID:      messageID,
		requestedModel: requestedModel,
		inputTokens:    inputTokens,
		activeToolIdx:  -1,
	}
}

func (t *StreamTranslator) InputTokens() int         { return t.inputTokens }
func (t *StreamTranslator) UsageSeen() bool          { return t.usageSeen }
func (t *StreamTranslator) OutputTokens() int        { return t.outputTokens }
func (t *StreamTranslator) ReasoningTokens() int     { return t.reasoningTokens }
func (t *StreamTranslator) CacheReadTokens() int     { return t.cacheReadTokens }
func (t *StreamTranslator) CacheCreationTokens() int { return t.cacheCreationTokens }

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
			t.usageSeen = true
		}
		events = append(events, buildMessageStart(t.messageID, t.requestedModel, t.inputTokens))
		t.state = stateStarted
	}

	if len(chunk.Choices) == 0 {
		if chunk.Usage != nil {
			t.inputTokens = chunk.Usage.PromptTokens
			t.usageSeen = true
			t.outputTokens = chunk.Usage.CompletionTokens
			if chunk.Usage.CompletionTokensDetails != nil {
				t.reasoningTokens = chunk.Usage.CompletionTokensDetails.ReasoningTokens
			}
			if chunk.Usage.PromptTokensDetails != nil {
				t.cacheReadTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			}
			t.cacheCreationTokens = chunk.Usage.CacheCreationTokens
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
				t.usageSeen = true
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
			t.cacheCreationTokens = chunk.Usage.CacheCreationTokens
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
			if prev := t.tools[tc.Index]; prev != nil && prev.id == tc.ID {
				continue // duplicate header for an already-tracked call
			}

			// Close any active text/thinking block. An open tool_use block is
			// NOT closed here: its argument fragments may still arrive, and
			// Anthropic blocks cannot be reopened once stopped.
			if t.state == stateBlockActive && t.activeBlockType != "" {
				events = append(events, contentBlockStop(t.blockIndex))
				t.blockIndex++
				t.activeBlockType = ""
				t.state = stateStarted
			}

			if t.tools == nil {
				t.tools = make(map[int]*toolStreamState)
			}
			t.tools[tc.Index] = &toolStreamState{id: tc.ID, name: tc.Function.Name}
			t.toolOrder = append(t.toolOrder, tc.Index)
			t.state = stateBlockActive

			// Open the block immediately only when no other tool block is
			// open; otherwise defer until closeActiveToolBlock flushes in order.
			if t.activeToolIdx < 0 {
				events = append(events, t.openToolBlock(tc.Index)...)
			}
		}

		// Arguments delta
		if tc.Function.Arguments != "" {
			tool, ok := t.tools[tc.Index]
			if !ok {
				continue
			}
			if t.activeToolIdx == tc.Index {
				events = append(events, toolUseArgumentsDelta(tool.blockIndex, tc.Function.Arguments))
			}
			if tool.args.Len() <= 1<<20 { // 1MB max per tool
				tool.args.WriteString(tc.Function.Arguments)
			}
		}
	}

	return events
}

// openToolBlock emits content_block_start for the tool and marks it active.
func (t *StreamTranslator) openToolBlock(idx int) []domain.SSEEvent {
	tool := t.tools[idx]
	tool.blockIndex = t.blockIndex
	tool.opened = true
	t.activeToolIdx = idx
	return []domain.SSEEvent{toolUseBlockStart(tool.blockIndex, tool.id, tool.name)}
}

// closeActiveToolBlock closes the open tool block and flushes any deferred
// tool blocks (with their buffered arguments) in first-seen order.
func (t *StreamTranslator) closeActiveToolBlock() []domain.SSEEvent {
	var events []domain.SSEEvent
	for _, idx := range t.toolOrder {
		tool := t.tools[idx]
		if tool.closed {
			continue
		}
		if !tool.opened {
			events = append(events, t.openToolBlock(idx)...)
			if args := tool.args.String(); args != "" {
				events = append(events, toolUseArgumentsDelta(tool.blockIndex, args))
			}
		}
		events = append(events, contentBlockStop(tool.blockIndex))
		tool.closed = true
		t.blockIndex++
	}
	t.activeToolIdx = -1
	if len(t.toolOrder) > 0 {
		t.state = stateStarted
	}
	return events
}

func (t *StreamTranslator) OnProviderDisconnect() []domain.SSEEvent {
	var events []domain.SSEEvent

	switch t.state {
	case stateBlockActive:
		if len(t.toolOrder) > 0 {
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
