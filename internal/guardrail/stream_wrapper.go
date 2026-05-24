package guardrail

import (
	"context"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/crosslink/internal/domain"
)

type StreamGuardrailWrapper struct {
	ch         <-chan domain.SSEChunk
	svc        *GuardrailService
	model      string
	apiKeyID   int64
	teamID     int64
	buf        strings.Builder
	windowSize int
	failOpen   bool
}

func NewStreamGuardrailWrapper(ch <-chan domain.SSEChunk, svc *GuardrailService, model string, apiKeyID, teamID int64) *StreamGuardrailWrapper {
	return &StreamGuardrailWrapper{
		ch:         ch,
		svc:        svc,
		model:      model,
		apiKeyID:   apiKeyID,
		teamID:     teamID,
		windowSize: 2048,
		failOpen:   svc.IsFailOpen(),
	}
}

type StreamResult struct {
	Chunk   *domain.SSEChunk
	Blocked *CheckResponse
	Done    bool
}

func (w *StreamGuardrailWrapper) Next(ctx context.Context) StreamResult {
	var chunk domain.SSEChunk
	var ok bool
	select {
	case chunk, ok = <-w.ch:
		if !ok {
			return StreamResult{Done: true}
		}
	case <-ctx.Done():
		return StreamResult{Done: true}
	}

		if chunk.Done {
		if w.buf.Len() > 0 {
			text := w.buf.String()
			result, err := w.svc.Check(ctx, &CheckRequest{
				Content:   text,
				Direction: DirectionResponse,
				Model:     w.model,
				APIKeyID:  w.apiKeyID,
				TeamID:    w.teamID,
			})
			if err != nil {
				if !w.failOpen {
					return StreamResult{Blocked: &CheckResponse{Blocked: true, Reason: "guardrail service unavailable"}}
				}
				slog.Warn("guardrail: stream check failed, fail-open", "error", err)
			} else if result != nil && result.Blocked {
				return StreamResult{Blocked: result}
			}
		}
		return StreamResult{Chunk: &chunk}
	}

	if chunk.Chunk == nil || len(chunk.Chunk.Choices) == 0 {
		return StreamResult{Chunk: &chunk}
	}

	content := chunk.Chunk.Choices[0].Delta.Content
	if content != "" {
		w.buf.WriteString(content)
	}

	if utf8.RuneCountInString(w.buf.String()) >= w.windowSize {
		text := w.buf.String()
		result, err := w.svc.Check(ctx, &CheckRequest{
			Content:   text,
			Direction: DirectionResponse,
			Model:     w.model,
			APIKeyID:  w.apiKeyID,
			TeamID:    w.teamID,
		})
			if err != nil {
				if !w.failOpen {
					return StreamResult{Blocked: &CheckResponse{Blocked: true, Reason: "guardrail service unavailable"}}
				}
				slog.Warn("guardrail: stream check failed, fail-open", "error", err)
			} else if result != nil && result.Blocked {
				return StreamResult{Blocked: result}
			}
			w.slide()
		}

	return StreamResult{Chunk: &chunk}
}

// CallbackStreamGuardrail provides sliding-window guardrail checking
// for callback-based streaming (e.g., Anthropic's func(event) bool pattern).
type CallbackStreamGuardrail struct {
	svc        *GuardrailService
	model      string
	apiKeyID   int64
	teamID     int64
	buf        strings.Builder
	windowSize int
	failOpen   bool
}

func NewCallbackStreamGuardrail(svc *GuardrailService, model string, apiKeyID, teamID int64) *CallbackStreamGuardrail {
	return &CallbackStreamGuardrail{
		svc:        svc,
		model:      model,
		apiKeyID:   apiKeyID,
		teamID:     teamID,
		windowSize: 2048,
		failOpen:   svc.IsFailOpen(),
	}
}

func (w *CallbackStreamGuardrail) CheckText(ctx context.Context, text string) (blocked bool, result *CheckResponse) {
	checkResult, err := w.svc.Check(ctx, &CheckRequest{
		Content:   text,
		Direction: DirectionResponse,
		Model:     w.model,
		APIKeyID:  w.apiKeyID,
		TeamID:    w.teamID,
	})
	if err != nil {
		if !w.failOpen {
			return true, &CheckResponse{Blocked: true, Reason: "guardrail service unavailable"}
		}
		slog.Warn("guardrail: stream check failed, fail-open", "error", err)
		return false, nil
	}
	if checkResult == nil || !checkResult.Blocked {
		return false, nil
	}
	return true, checkResult
}

func (w *CallbackStreamGuardrail) FinalCheck(ctx context.Context, text string) (blocked bool, result *CheckResponse) {
	checkResult, err := w.svc.Check(ctx, &CheckRequest{
		Content:   text,
		Direction: DirectionResponse,
		Model:     w.model,
		APIKeyID:  w.apiKeyID,
		TeamID:    w.teamID,
	})
	if err != nil {
		if !w.failOpen {
			return true, &CheckResponse{Blocked: true, Reason: "guardrail service unavailable"}
		}
		slog.Warn("guardrail: stream final check failed, fail-open", "error", err)
		return false, nil
	}
	if checkResult == nil || !checkResult.Blocked {
		return false, nil
	}
	return true, checkResult
}

func (w *CallbackStreamGuardrail) Append(text string) {
	w.buf.WriteString(text)
}

func (w *CallbackStreamGuardrail) BufferLen() int      { return utf8.RuneCountInString(w.buf.String()) }
func (w *CallbackStreamGuardrail) BufferText() string  { return w.buf.String() }
func (w *CallbackStreamGuardrail) WindowSize() int     { return w.windowSize }

func (w *CallbackStreamGuardrail) Slide() { slideBuffer(&w.buf, w.windowSize) }

func (w *StreamGuardrailWrapper) slide() { slideBuffer(&w.buf, w.windowSize) }

func slideBuffer(buf *strings.Builder, windowSize int) {
	text := buf.String()
	runes := []rune(text)
	halfRunes := windowSize / 2
	if len(runes) > halfRunes {
		buf.Reset()
		buf.WriteString(string(runes[len(runes)-halfRunes:]))
	}
}
