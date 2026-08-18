package service

import (
	"encoding/json"
	"strings"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/translator"
	"github.com/crosslink/pkg/token"
)

// ContextBuckets is the five-bucket token composition of one request.
// All values are estimates (pkg/token), model-independent.
type ContextBuckets struct {
	SystemTokens     int `json:"system_tokens"`
	HistoryTokens    int `json:"history_tokens"`
	QuestionTokens   int `json:"question_tokens"`
	ToolTokens       int `json:"tool_tokens"`
	ToolOutputTokens int `json:"tool_output_tokens"`
	MessageCount     int `json:"message_count"`
}

func (b ContextBuckets) Total() int {
	return b.SystemTokens + b.HistoryTokens + b.QuestionTokens + b.ToolTokens + b.ToolOutputTokens
}

// Analysis flag bits (design §4.2). Low bits first, high bits reserved for
// phase-B intervention actions.
const (
	FlagOverflowRisk   = 1 << 0 // context + max_tokens > window
	FlagLongHistory    = 1 << 1 // history > 60% window
	FlagLongToolOutput = 1 << 2 // tool_outputs > 40% window
	FlagWindowUnknown  = 1 << 3 // MaxContext unavailable
	FlagUnanalyzed     = 1 << 4 // analysis failed / cache-hit request
)

// AnalyzeAnthropicBuckets splits an Anthropic request into five token buckets.
// Rules (design §3.2): tool_result blocks (incl. those inside the last user
// message) go to tool_outputs; the last user message's text blocks go to
// question; everything else is history. tool_use input JSON counts as history.
func AnalyzeAnthropicBuckets(req *domain.AnthropicRequest) ContextBuckets {
	var b ContextBuckets
	b.SystemTokens = token.EstimateZeroAlloc(translator.ExtractContentText(req.System))
	if len(req.Tools) > 0 {
		b.ToolTokens = token.EstimateZeroAlloc(string(req.Tools))
	}
	b.MessageCount = len(req.Messages)

	// Only a trailing user message is the "question"; if the last message is
	// assistant (incl. tool_use) or a tool result, earlier user turns are history.
	trailingUser := -1
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == "user" {
		trailingUser = n - 1
	}

	for i := range req.Messages {
		blocks, ok := parseAnthropicBlocks(req.Messages[i].Content)
		if !ok {
			// Plain string content.
			txt := translator.ExtractContentText(req.Messages[i].Content)
			if i == trailingUser {
				b.QuestionTokens += token.EstimateZeroAlloc(txt)
			} else {
				b.HistoryTokens += token.EstimateZeroAlloc(txt)
			}
			continue
		}
		for _, blk := range blocks {
			switch {
			case blk.Type == "tool_result":
				// Nested content may be string or block array.
				nested := translator.ExtractContentText(blk.Content)
				b.ToolOutputTokens += token.EstimateZeroAlloc(nested)
			case i == trailingUser && blk.Type == "text":
				b.QuestionTokens += token.EstimateZeroAlloc(blk.Text)
			case blk.Type == "tool_use":
				b.HistoryTokens += token.EstimateZeroAlloc(string(blk.Input)) + token.EstimateZeroAlloc(blk.Name)
			default:
				if blk.Text != "" {
					b.HistoryTokens += token.EstimateZeroAlloc(blk.Text)
				}
			}
		}
	}
	return b
}

// AnalyzeOpenAIBuckets splits an OpenAI request into five token buckets.
// Rules: role=system -> system; role=tool -> tool_outputs; last user message
// -> question (text only); assistant tool_calls JSON -> history; rest -> history.
func AnalyzeOpenAIBuckets(req *domain.OpenAIRequest) ContextBuckets {
	var b ContextBuckets
	b.MessageCount = len(req.Messages)

	// Only a trailing user message is the "question"; if the last message is
	// role=tool (agentic mid-loop) earlier user turns are history.
	lastUser := -1
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == "user" {
		lastUser = n - 1
	}

	for i, msg := range req.Messages {
		txt := domain.ContentText(msg.Content)
		switch {
		case msg.Role == "system":
			b.SystemTokens += token.EstimateZeroAlloc(txt)
		case msg.Role == "tool":
			b.ToolOutputTokens += token.EstimateZeroAlloc(txt)
		case i == lastUser:
			b.QuestionTokens += token.EstimateZeroAlloc(txt)
		default:
			b.HistoryTokens += token.EstimateZeroAlloc(txt)
		}
		for _, tc := range msg.ToolCalls {
			b.HistoryTokens += token.EstimateZeroAlloc(tc.Function.Name) + token.EstimateZeroAlloc(tc.Function.Arguments)
		}
	}
	for _, tool := range req.Tools {
		raw, _ := json.Marshal(tool)
		b.ToolTokens += token.EstimateZeroAlloc(string(raw))
	}
	return b
}

// ComputeAnalysisFlags derives risk flags and utilization (basis points,
// 10000 = 100%). window == 0 means unknown: bp = -1 sentinel, FlagWindowUnknown.
func ComputeAnalysisFlags(b ContextBuckets, window, maxTokens int) (flags int, bp int) {
	if window <= 0 {
		return FlagWindowUnknown, -1
	}
	total := b.Total()
	bp = total * 10000 / window
	if total+maxTokens > window {
		flags |= FlagOverflowRisk
	}
	if b.HistoryTokens*100 > window*60 {
		flags |= FlagLongHistory
	}
	if b.ToolOutputTokens*100 > window*40 {
		flags |= FlagLongToolOutput
	}
	return flags, bp
}

func parseAnthropicBlocks(raw json.RawMessage) ([]domain.ContentBlock, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var blocks []domain.ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, false
	}
	return blocks, true
}

// defaultContextWindows maps model-name prefixes to conservative public
// context windows. Longest-prefix match wins. 0 = not found.
var defaultContextWindows = []struct {
	prefix string
	window int
}{
	{"claude-opus-4", 200000},
	{"claude-sonnet-4", 200000},
	{"claude-3-7", 200000},
	{"claude-3-5", 200000},
	{"claude-3-haiku", 200000},
	{"gpt-5", 400000},
	{"gpt-4.1", 1000000},
	{"gpt-4o", 128000},
	{"o1", 200000},
	{"o3", 200000},
	{"deepseek-chat", 64000},
	{"deepseek-reasoner", 64000},
	{"qwen-max", 32000},
	{"qwen-plus", 131000},
	{"glm-4", 128000},
	{"minimax", 200000},
	{"gemini-2.5-pro", 1000000},
	{"gemini-2.5-flash", 1000000},
}

func DefaultContextWindow(modelName string) int {
	// Provider models are stored with vendor casing (GLM-4.7-Flash,
	// MiniMax-M2.7) while prefixes are lowercase — compare case-insensitively.
	lower := strings.ToLower(modelName)
	best, bestLen := 0, 0
	for _, e := range defaultContextWindows {
		if len(e.prefix) > bestLen && strings.HasPrefix(lower, e.prefix) {
			best, bestLen = e.window, len(e.prefix)
		}
	}
	return best
}
