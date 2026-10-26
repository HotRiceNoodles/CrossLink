package guardrail

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"strings"
	"time"
	"unicode/utf8"

	"github.com/crosslink/internal/domain"
)

// Stream guardrail execution modes (GuardrailService.StreamMode):
//   - "window" (default, legacy): content is checked in a sliding window as it
//     accumulates; chunks are returned to the caller as they arrive, so up to
//     one window of violating content may already have been emitted when a
//     check blocks. Compatibility mode — existing deployments keep this.
//   - "strict": NOTHING is emitted until the final check passes. The wrapper
//     buffers the whole response, reassembling each logical field (per choice
//     index / tool-call index for OpenAI, per block for Anthropic) before
//     checking, so patterns split across interleaved chunks are still seen
//     contiguous. Buffering is bounded by StrictMaxRunes (extracted text),
//     StrictMaxBytes (raw buffered estimate) and StrictMaxEvents (chunk/event
//     count); any overflow fails closed. Mask results are applied by Action,
//     not by Blocked — a mask that cannot be safely applied (no spans, or a
//     masked tool-arguments string is no longer valid JSON) fails closed too.

type StreamGuardrailWrapper struct {
	ch         <-chan domain.SSEChunk
	svc        *GuardrailService
	model      string
	apiKeyID   int64
	teamID     int64
	orgID      int64
	turnPrompt string
	buf        strings.Builder
	windowSize int
	failOpen   bool

	strict          bool
	strictMaxRunes  int
	strictMaxBytes  int
	strictMaxEvents int
	pending         []domain.SSEChunk
	groups          map[string]*streamGroup
	groupOrder      []string
	runes           int
	bytes           int
	cleared         []domain.SSEChunk
	blockedResult   *CheckResponse
	decided         bool
}

// streamGroup reassembles one logical streamed field (one choice's content,
// one tool call's arguments, one choice's reasoning).
type streamGroup struct {
	text   strings.Builder
	frags  []streamFrag
	family int // 0 = text family, 1 = JSON-arguments family, 2 = thinking family
}

// fragmentFamily maps a fragment kind to its reassembly family so a block's
// initial content (content_block_start) and its deltas (content_block_delta)
// land in the SAME group: 0/3 text, 1/5 JSON args, 2/4 thinking.
func fragmentFamily(kind int) int {
	switch kind {
	case 1, 5:
		return 1
	case 2, 4:
		return 2
	default:
		return 0
	}
}

// streamFrag locates one chunk fragment inside its group's accumulated text.
type streamFrag struct {
	chunkIdx int
	choice   int // index into chunk.Chunk.Choices
	toolIdx  int // index into delta.ToolCalls; -1 otherwise
	kind     int // 0=content, 1=tool arguments, 2=reasoning
	start    int // offset within the group text
	end      int
}

func NewStreamGuardrailWrapper(ch <-chan domain.SSEChunk, svc *GuardrailService, model string, apiKeyID, teamID, orgID int64) *StreamGuardrailWrapper {
	w := &StreamGuardrailWrapper{
		ch:         ch,
		svc:        svc,
		model:      model,
		apiKeyID:   apiKeyID,
		teamID:     teamID,
		orgID:      orgID,
		windowSize: 2048,
	}
	if svc != nil {
		w.failOpen = svc.IsFailOpen()
		w.strict = svc.StreamMode() == "strict"
		w.strictMaxRunes = svc.StrictMaxRunes()
		w.strictMaxBytes = svc.StrictMaxBytes()
		w.strictMaxEvents = svc.StrictMaxEvents()
	}
	return w
}

// SetTurnPrompt records the original user input so response-direction checks
// can judge the streamed assistant output in context (engines that don't need
// it ignore the value; call before the first Next).
func (w *StreamGuardrailWrapper) SetTurnPrompt(p string) { w.turnPrompt = p }

// SetTurnPrompt is the CallbackStreamGuardrail counterpart of
// StreamGuardrailWrapper.SetTurnPrompt.
func (w *CallbackStreamGuardrail) SetTurnPrompt(p string) { w.turnPrompt = p }

type StreamResult struct {
	Chunk   *domain.SSEChunk
	Blocked *CheckResponse
	Done    bool
}

func (w *StreamGuardrailWrapper) Strict() bool { return w.strict }

func (w *StreamGuardrailWrapper) Next(ctx context.Context) StreamResult {
	if w.strict {
		return w.nextStrict(ctx)
	}
	return w.nextWindow(ctx)
}

// nextStrict holds every chunk until the stream completes, then runs a single
// final check. Nothing unchecked is ever returned to the caller.
func (w *StreamGuardrailWrapper) nextStrict(ctx context.Context) StreamResult {
	for {
		if len(w.cleared) > 0 {
			c := w.cleared[0]
			w.cleared = w.cleared[1:]
			return StreamResult{Chunk: &c}
		}
		if w.blockedResult != nil {
			return StreamResult{Blocked: w.blockedResult}
		}
		if w.decided {
			return StreamResult{Done: true}
		}
		var chunk domain.SSEChunk
		var ok bool
		select {
		case chunk, ok = <-w.ch:
			if !ok {
				// Upstream channel closed without a Done marker: the final
				// check on what accumulated is still a decision point, not a
				// bypass.
				w.finalizeStrict(ctx, nil)
				continue
			}
		case <-ctx.Done():
			// Caller context cancelled (client gone / upstream cancelled):
			// drop buffered content. Unchecked content is never emitted.
			w.decided = true
			w.pending = nil
			return StreamResult{Done: true}
		}

		if chunk.Done {
			w.finalizeStrict(ctx, &chunk)
			continue
		}
		size := w.recordChunk(chunk)
		w.pending = append(w.pending, chunk)
		w.bytes += size
		if w.runes > w.strictMaxRunes || w.bytes > w.strictMaxBytes || len(w.pending) > w.strictMaxEvents {
			// Buffer cap exceeded: fail closed regardless of fail-open —
			// strict mode must not release unchecked content, and the buffer
			// cannot grow further.
			slog.Error("guardrail: strict stream buffer cap exceeded",
				"runes", w.runes, "runes_limit", w.strictMaxRunes,
				"bytes", w.bytes, "bytes_limit", w.strictMaxBytes,
				"events", len(w.pending), "events_limit", w.strictMaxEvents,
				"model", w.model)
			w.decided = true
			w.pending = nil
			w.blockedResult = &CheckResponse{
				Blocked:  true,
				Action:   "block",
				RuleName: "guardrail_stream_cap",
				Reason:   "streamed response exceeds strict-mode guardrail buffer limit",
			}
		}
	}
}

// chunkCost measures the true buffered size of one chunk (its serialized JSON
// length) for the byte cap — envelope fields like a large chunk ID count too,
// not only extracted text.
func chunkCost(chunk domain.SSEChunk) int {
	if chunk.Chunk == nil {
		return 64
	}
	if b, err := json.Marshal(chunk.Chunk); err == nil {
		return len(b) + 48 // + SSE event framing estimate
	}
	return 256
}

// recordChunk reassembles checkable text into per-field groups keyed by the
// wire choice index, kind and wire tool-call index, so interleaved chunks of
// different choices/tool calls never split each other's text. It returns the
// chunk's size estimate.
func (w *StreamGuardrailWrapper) recordChunk(chunk domain.SSEChunk) int {
	if w.groups == nil {
		w.groups = make(map[string]*streamGroup)
	}
	if chunk.Chunk == nil {
		return chunkCost(chunk)
	}
	idx := len(w.pending)
	for ci := range chunk.Chunk.Choices {
		choice := &chunk.Chunk.Choices[ci]
		delta := &choice.Delta
		wireIdx := choice.Index
		if delta.Content != "" {
			w.addFrag(fmt.Sprintf("c%d:0", wireIdx), streamFrag{chunkIdx: idx, choice: ci, toolIdx: -1, kind: 0}, delta.Content)
		}
		if delta.ReasoningContent != "" {
			w.addFrag(fmt.Sprintf("c%d:2", wireIdx), streamFrag{chunkIdx: idx, choice: ci, toolIdx: -1, kind: 2}, delta.ReasoningContent)
		}
		for ti := range delta.ToolCalls {
			args := delta.ToolCalls[ti].Function.Arguments
			if args == "" {
				continue
			}
			w.addFrag(fmt.Sprintf("c%d:1:t%d", wireIdx, delta.ToolCalls[ti].Index), streamFrag{chunkIdx: idx, choice: ci, toolIdx: ti, kind: 1}, args)
		}
	}
	return chunkCost(chunk)
}

func (w *StreamGuardrailWrapper) addFrag(key string, frag streamFrag, text string) {
	g := w.groups[key]
	if g == nil {
		g = &streamGroup{family: fragmentFamily(frag.kind)}
		w.groups[key] = g
		w.groupOrder = append(w.groupOrder, key)
	}
	frag.start = g.text.Len()
	frag.end = frag.start + len(text)
	g.text.WriteString(text)
	g.frags = append(g.frags, frag)
	w.runes += utf8.RuneCountInString(text)
}

// canonicalJSONString decodes and re-encodes a JSON document so escaped
// sequences (\uXXXX) appear decoded; empty string when not valid JSON.
func canonicalJSONString(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// groupDetectionText returns the text a group contributes to detection.
// Tool-argument groups are canonicalized (escapes decoded) when the complete
// reassembled string is valid JSON — otherwise the raw text is used as-is.
func groupDetectionText(g *streamGroup) string {
	t := g.text.String()
	if g.family == 1 {
		if c := canonicalJSONString(t); c != "" {
			return c
		}
	}
	return t
}

// joinedGroups returns the per-group DETECTION texts (tool-argument groups
// canonicalized) in first-appearance order, their base offsets in the joined
// detection text, and the joined text itself. bases are relative to the
// canonical texts — applyMask must use the same lengths.
func (w *StreamGuardrailWrapper) joinedGroups() (joined string, bases map[string]int, texts map[string]string) {
	texts = make(map[string]string, len(w.groupOrder))
	bases = make(map[string]int, len(w.groupOrder))
	list := make([]string, 0, len(w.groupOrder))
	total := 0
	for _, k := range w.groupOrder {
		t := groupDetectionText(w.groups[k])
		texts[k] = t
		bases[k] = total
		list = append(list, t)
		total += len(t) + 1 // +1 for the "\n" separator
	}
	joined = strings.Join(list, "\n")
	return joined, bases, texts
}

// finalizeStrict runs the final check on the reassembled content and moves the
// buffered chunks into the release queue (masked when the action is mask).
// doneChunk, when non-nil, is the stream's [DONE] marker and is appended last.
func (w *StreamGuardrailWrapper) finalizeStrict(ctx context.Context, doneChunk *domain.SSEChunk) {
	w.decided = true
	if doneChunk != nil {
		w.pending = append(w.pending, *doneChunk)
	}
	joined, bases, gtexts := w.joinedGroups()
	if len(joined) == 0 {
		// Nothing checkable accumulated — control chunks carry no generated
		// content, so releasing them is vacuously safe.
		w.cleared = append(w.cleared, w.pending...)
		w.pending = nil
		return
	}

	checkCtx := ctx
	if w.svc != nil {
		if budget := w.svc.ResponseBudgetMs(); budget > 0 {
			var cancel context.CancelFunc
			checkCtx, cancel = context.WithTimeout(ctx, time.Duration(budget)*time.Millisecond)
			defer cancel()
		}
	}
	result, err := w.svc.Check(checkCtx, &CheckRequest{
		Content:   joined,
		Direction: DirectionResponse,
		Model:     w.model,
		APIKeyID:  w.apiKeyID,
		TeamID:    w.teamID,
		OrgID:     w.orgID,
		Prompt:    w.turnPrompt,
	})
	if err != nil {
		if !w.failOpen {
			slog.Error("guardrail: strict stream final check failed, blocking", "error", err)
			w.pending = nil
			w.blockedResult = &CheckResponse{Blocked: true, RuleName: "guardrail_unavailable", Reason: "guardrail service unavailable"}
			return
		}
		slog.Warn("guardrail: strict stream final check failed, fail-open", "error", err)
		w.cleared = append(w.cleared, w.pending...)
		w.pending = nil
		return
	}
	if result != nil && result.Action == "mask" {
		// Mask is decided by Action, NOT by Blocked: a mask response with
		// Blocked=false still must never release unmasked content.
		if len(result.MaskedSpans) == 0 {
			slog.Warn("guardrail: strict stream mask without spans cannot be applied, blocking", "rule", result.RuleName)
			w.pending = nil
			w.blockedResult = &CheckResponse{Blocked: true, RuleName: result.RuleName, Reason: result.Reason}
			return
		}
		if controlled := w.applyMask(joined, bases, gtexts, result.MaskedSpans); controlled != nil {
			w.pending = nil
			w.blockedResult = controlled
			return
		}
		w.cleared = append(w.cleared, w.pending...)
		w.pending = nil
		return
	}
	if result != nil && result.Blocked {
		w.pending = nil
		w.blockedResult = result
		return
	}
	w.cleared = append(w.cleared, w.pending...)
	w.pending = nil
}

// applyMask patches buffered chunks for every group fragment intersecting a
// span. Tool-argument groups are masked as one complete string (fragments
// after the first are emptied) and the masked result must stay valid JSON —
// otherwise a controlled block decision is returned.
func (w *StreamGuardrailWrapper) applyMask(joined string, bases map[string]int, texts map[string]string, spans []Span) *CheckResponse {
	for key, g := range w.groups {
		base := bases[key]
		if g.family != 1 {
			for _, frag := range g.frags {
				masked := maskRange(joined, spans, base+frag.start, base+frag.end)
				if masked == nil {
					continue
				}
				w.setFragText(frag, *masked)
			}
			continue
		}
		// Tool arguments: mask the complete reassembled string (detection text
		// is the canonical/decoded form), validate the whole JSON, then write
		// the masked canonical form into the first fragment and empty the rest.
		detection := texts[key]
		complete := maskRange(joined, spans, base, base+len(detection))
		if complete == nil {
			continue
		}
		if !json.Valid([]byte(*complete)) {
			slog.Warn("guardrail: masked tool arguments no longer valid JSON, blocking stream", "group", key)
			return &CheckResponse{
				Blocked:  true,
				Action:   "block",
				RuleName: "guardrail_mask_invalid",
				Reason:   "masked tool-call arguments would no longer be valid JSON; refusing to emit unmasked arguments",
			}
		}
		for i, frag := range g.frags {
			if i == 0 {
				w.setFragText(frag, *complete)
			} else {
				w.setFragText(frag, "")
			}
		}
	}
	return nil
}

func (w *StreamGuardrailWrapper) setFragText(frag streamFrag, text string) {
	if frag.chunkIdx >= len(w.pending) {
		return
	}
	chunk := w.pending[frag.chunkIdx].Chunk
	if chunk == nil || frag.choice >= len(chunk.Choices) {
		return
	}
	delta := &chunk.Choices[frag.choice].Delta
	switch frag.kind {
	case 0:
		delta.Content = text
	case 2:
		delta.ReasoningContent = text
	case 1:
		if frag.toolIdx >= 0 && frag.toolIdx < len(delta.ToolCalls) {
			delta.ToolCalls[frag.toolIdx].Function.Arguments = text
		}
	}
}

// maskRange applies non-overlapping spans to the [start,end) fragment of
// content and returns the masked fragment, or nil when no span touches it.
func maskRange(content string, spans []Span, start, end int) *string {
	var b strings.Builder
	touched := false
	last := start
	for _, sp := range spans {
		if sp.End <= start || sp.Start >= end {
			continue
		}
		touched = true
		s := sp.Start
		if s < start {
			s = start
		}
		e := sp.End
		if e > end {
			e = end
		}
		if s > last {
			b.WriteString(content[last:s])
		}
		b.WriteString(sp.Replacement)
		last = e
	}
	if !touched {
		return nil
	}
	if last < end {
		b.WriteString(content[last:end])
	}
	out := b.String()
	return &out
}

// nextWindow is the legacy sliding-window mode: chunks are returned as they
// arrive and the accumulated buffer (all choices' content, reasoning and tool
// arguments, in arrival order) is checked every windowSize runes. Up to one
// window of violating content may already have been emitted when a check
// fires — this is the documented compatibility trade-off of window mode.
// Mask actions cannot retract already-emitted text and are treated as pass.
func (w *StreamGuardrailWrapper) nextWindow(ctx context.Context) StreamResult {
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
				OrgID:     w.orgID,
				Prompt:    w.turnPrompt,
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

	// Accumulate content from ALL choices plus tool arguments and reasoning.
	for ci := range chunk.Chunk.Choices {
		delta := &chunk.Chunk.Choices[ci].Delta
		if delta.Content != "" {
			w.buf.WriteString(delta.Content)
		}
		if delta.ReasoningContent != "" {
			w.buf.WriteString(delta.ReasoningContent)
		}
		for ti := range delta.ToolCalls {
			if args := delta.ToolCalls[ti].Function.Arguments; args != "" {
				w.buf.WriteString(args)
			}
		}
	}

	if utf8.RuneCountInString(w.buf.String()) >= w.windowSize {
		text := w.buf.String()
		result, err := w.svc.Check(ctx, &CheckRequest{
			Content:   text,
			Direction: DirectionResponse,
			Model:     w.model,
			APIKeyID:  w.apiKeyID,
			TeamID:    w.teamID,
			OrgID:     w.orgID,
			Prompt:    w.turnPrompt,
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

// CallbackStreamGuardrail provides guardrail checking for callback-based
// streaming (Anthropic's func(event) bool pattern).
type CallbackStreamGuardrail struct {
	svc        *GuardrailService
	model      string
	apiKeyID   int64
	teamID     int64
	orgID      int64
	turnPrompt string
	buf        strings.Builder
	windowSize int
	failOpen   bool

	strict          bool
	strictMaxRunes  int
	strictMaxBytes  int
	strictMaxEvents int
	runes           int
	bytes           int
	// Strict-mode buffering: raw (event, data) pairs, plus per-block group
	// reassembly (block index distinguishes interleaved content blocks).
	events     []callbackEvent
	groups     map[string]*streamGroup
	groupOrder []string
}

type callbackEvent struct {
	name string
	data string
}

// Name and Data expose the buffered raw SSE event to release callers.
func (e callbackEvent) Name() string { return e.name }
func (e callbackEvent) Data() string { return e.data }

func NewCallbackStreamGuardrail(svc *GuardrailService, model string, apiKeyID, teamID, orgID int64) *CallbackStreamGuardrail {
	w := &CallbackStreamGuardrail{
		svc:        svc,
		model:      model,
		apiKeyID:   apiKeyID,
		teamID:     teamID,
		orgID:      orgID,
		windowSize: 2048,
	}
	if svc != nil {
		w.failOpen = svc.IsFailOpen()
		w.strict = svc.StreamMode() == "strict"
		w.strictMaxRunes = svc.StrictMaxRunes()
		w.strictMaxBytes = svc.StrictMaxBytes()
		w.strictMaxEvents = svc.StrictMaxEvents()
	}
	return w
}

func (w *CallbackStreamGuardrail) Strict() bool { return w.strict }

// Append records streamed text (window-mode accumulation buffer).
func (w *CallbackStreamGuardrail) Append(text string) {
	w.buf.WriteString(text)
}

func (w *CallbackStreamGuardrail) BufferLen() int     { return utf8.RuneCountInString(w.buf.String()) }
func (w *CallbackStreamGuardrail) BufferText() string { return w.buf.String() }
func (w *CallbackStreamGuardrail) WindowSize() int    { return w.windowSize }

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

// --- Anthropic callback strict mode ---

// BufferEvent stores one raw SSE event (name + serialized data) in strict
// mode and enforces the event-count/byte caps. Returns false when a cap is
// exceeded — the caller must fail closed.
func (w *CallbackStreamGuardrail) BufferEvent(name, data string) bool {
	w.events = append(w.events, callbackEvent{name: name, data: data})
	w.bytes += len(data) + 48
	return w.bytes <= w.strictMaxBytes && len(w.events) <= w.strictMaxEvents
}

// RecordDelta accumulates one checkable fragment into its per-block group
// (blockKey identifies the Anthropic content block / tool input stream) and
// enforces the rune cap. Returns false when the cap is exceeded.
func (w *CallbackStreamGuardrail) RecordDelta(blockKey string, kind int, text string) bool {
	if text == "" {
		return w.overflown()
	}
	if w.groups == nil {
		w.groups = make(map[string]*streamGroup)
	}
	// Family-based grouping merges a block's initial content (kinds 3/4/5,
	// from content_block_start) with its deltas (kinds 0/1/2).
	key := fmt.Sprintf("%s:f%d", blockKey, fragmentFamily(kind))
	if kind == 5 {
		// Initial input is a complete object, not a prefix of JSON deltas.
		key += ":initial"
	}
	g := w.groups[key]
	if g == nil {
		g = &streamGroup{family: fragmentFamily(kind)}
		w.groups[key] = g
		w.groupOrder = append(w.groupOrder, key)
	}
	// Fragments reference event indexes; the payload field patched on release
	// depends on kind (delta.text / delta.partial_json / delta.thinking, or
	// content_block.text / content_block.input / content_block.thinking).
	g.text.WriteString(text)
	g.frags = append(g.frags, streamFrag{chunkIdx: len(w.events) - 1, kind: kind, start: g.text.Len() - len(text), end: g.text.Len()})
	w.runes += utf8.RuneCountInString(text)
	return w.overflown()
}

func (w *CallbackStreamGuardrail) overflown() bool {
	return w.runes > w.strictMaxRunes || w.bytes > w.strictMaxBytes || len(w.events) > w.strictMaxEvents
}

type StrictDecision int

const (
	StrictRelease StrictDecision = iota
	StrictBlock
	StrictMasked
	StrictFailOpen
)

// StrictFinish runs the final check over the reassembled groups and returns
// the decision plus the (possibly masked) events to emit. On StrictBlock,
// events are nil — callers must emit nothing but the guardrail error. No
// event may be written to the client before this verdict in strict mode.
func (w *CallbackStreamGuardrail) StrictFinish(ctx context.Context) (StrictDecision, []callbackEvent, *CheckResponse) {
	joined, bases, gtexts := w.joinedGroups()
	if len(joined) == 0 {
		return StrictRelease, w.events, nil
	}
	checkCtx := ctx
	if w.svc != nil {
		if budget := w.svc.ResponseBudgetMs(); budget > 0 {
			var cancel context.CancelFunc
			checkCtx, cancel = context.WithTimeout(ctx, time.Duration(budget)*time.Millisecond)
			defer cancel()
		}
	}
	result, err := w.svc.Check(checkCtx, &CheckRequest{
		Content:   joined,
		Direction: DirectionResponse,
		Model:     w.model,
		APIKeyID:  w.apiKeyID,
		TeamID:    w.teamID,
		OrgID:     w.orgID,
		Prompt:    w.turnPrompt,
	})
	if err != nil {
		if !w.failOpen {
			return StrictBlock, nil, &CheckResponse{Blocked: true, RuleName: "guardrail_unavailable", Reason: "guardrail service unavailable"}
		}
		slog.Warn("guardrail: strict stream final check failed, fail-open", "error", err)
		return StrictFailOpen, w.events, nil
	}
	if result != nil && result.Action == "mask" {
		if len(result.MaskedSpans) == 0 {
			slog.Warn("guardrail: strict stream mask without spans cannot be applied, blocking", "rule", result.RuleName)
			return StrictBlock, nil, &CheckResponse{Blocked: true, RuleName: result.RuleName, Reason: result.Reason}
		}
		if controlled := w.applyMask(joined, bases, gtexts, result.MaskedSpans); controlled != nil {
			return StrictBlock, nil, controlled
		}
		return StrictMasked, w.events, result
	}
	if result != nil && result.Blocked {
		return StrictBlock, nil, result
	}
	return StrictRelease, w.events, nil
}

func (w *CallbackStreamGuardrail) joinedGroups() (joined string, bases map[string]int, texts map[string]string) {
	texts = make(map[string]string, len(w.groupOrder))
	list := make([]string, 0, len(w.groupOrder))
	bases = make(map[string]int, len(w.groupOrder))
	total := 0
	for _, k := range w.groupOrder {
		t := groupDetectionText(w.groups[k])
		texts[k] = t
		bases[k] = total
		list = append(list, t)
		total += len(t) + 1
	}
	return strings.Join(list, "\n"), bases, texts
}

// applyMask patches buffered event payloads for every group fragment
// intersecting a span. partial_json (tool input) groups are masked as the
// complete reassembled JSON — first fragment carries it, the rest are
// emptied — and must remain valid JSON, otherwise a controlled block is
// returned.
func (w *CallbackStreamGuardrail) applyMask(joined string, bases map[string]int, texts map[string]string, spans []Span) *CheckResponse {
	for key, g := range w.groups {
		base := bases[key]
		if g.family != 1 {
			for _, frag := range g.frags {
				masked := maskRange(joined, spans, base+frag.start, base+frag.end)
				if masked == nil {
					continue
				}
				w.patchEvent(frag.chunkIdx, frag.kind, *masked, false)
			}
			continue
		}
		complete := maskRange(joined, spans, base, base+len(texts[key]))
		if complete == nil && !w.hasFamilyFrag(g, 5) {
			continue
		}
		// kind 5 fragments (content_block_start.input) are complete JSON
		// objects on their own — mask each individually; patchEvent validates.
		for _, frag := range g.frags {
			if frag.kind != 5 {
				continue
			}
			if complete != nil {
				if !json.Valid([]byte(*complete)) {
					return &CheckResponse{Blocked: true, Action: "block", RuleName: "guardrail_mask_invalid", Reason: "masked tool input is invalid JSON"}
				}
				w.patchEvent(frag.chunkIdx, 5, *complete, false)
			}
		}
		// kind 1 fragments (input_json_delta) concatenate into the real
		// arguments JSON: mask the complete delta range, validate, write into
		// the first fragment and clear the rest.
		var k1 []streamFrag
		for _, frag := range g.frags {
			if frag.kind == 1 {
				k1 = append(k1, frag)
			}
		}
		if len(k1) == 0 {
			continue
		}
		maskedDeltas := complete
		if maskedDeltas == nil {
			continue
		}
		if !json.Valid([]byte(*maskedDeltas)) {
			slog.Warn("guardrail: masked tool input no longer valid JSON, blocking stream", "group", key)
			return &CheckResponse{
				Blocked:  true,
				Action:   "block",
				RuleName: "guardrail_mask_invalid",
				Reason:   "masked tool-call input would no longer be valid JSON; refusing to emit unmasked input",
			}
		}
		for i, frag := range k1 {
			if i == 0 {
				w.patchEvent(frag.chunkIdx, 1, *maskedDeltas, false)
			} else {
				w.patchEvent(frag.chunkIdx, 1, "", true)
			}
		}
	}
	return nil
}

func (w *CallbackStreamGuardrail) hasFamilyFrag(g *streamGroup, kind int) bool {
	for _, frag := range g.frags {
		if frag.kind == kind {
			return true
		}
	}
	return false
}

// patchEvent rewrites the text field of a buffered event payload. Kinds 0-2
// address delta fields (delta.text / delta.partial_json / delta.thinking);
// kinds 3-5 address content_block_start fields (content_block.text /
// content_block.thinking / content_block.input — JSON validated). When clear
// is true the field is set to "" without requiring it to exist.
func (w *CallbackStreamGuardrail) patchEvent(eventIdx, kind int, text string, clear bool) {
	if eventIdx < 0 || eventIdx >= len(w.events) {
		return
	}
	ev := &w.events[eventIdx]
	var payload map[string]any
	if json.Unmarshal([]byte(ev.data), &payload) != nil {
		return
	}
	switch kind {
	case 0, 1, 2:
		delta, ok := payload["delta"].(map[string]any)
		if !ok {
			if !clear {
				return
			}
			delta = map[string]any{}
			payload["delta"] = delta
		}
		field := ""
		switch kind {
		case 0:
			field = "text"
		case 1:
			field = "partial_json"
		case 2:
			field = "thinking"
		}
		if _, exists := delta[field]; !exists && !clear {
			return
		}
		delta[field] = text
	case 3, 4, 5:
		block, ok := payload["content_block"].(map[string]any)
		if !ok {
			if !clear {
				return
			}
			block = map[string]any{}
			payload["content_block"] = block
		}
		field := ""
		switch kind {
		case 3:
			field = "text"
		case 4:
			field = "thinking"
		case 5:
			field = "input"
		}
		if _, exists := block[field]; !exists && !clear {
			return
		}
		if kind == 5 {
			// content_block.input is a JSON object; the masked replacement
			// must stay valid JSON.
			if clear {
				block[field] = map[string]any{}
			} else if json.Valid([]byte(text)) {
				var v any
				if json.Unmarshal([]byte(text), &v) == nil {
					block[field] = v
				} else {
					return
				}
			} else {
				return
			}
		} else if clear {
			block[field] = ""
		} else {
			block[field] = text
		}
	}
	if b, err := json.Marshal(payload); err == nil {
		ev.data = string(b)
	}
}

// CheckText runs a window-mode check on the given text.
func (w *CallbackStreamGuardrail) CheckText(ctx context.Context, text string) (blocked bool, result *CheckResponse) {
	checkResult, err := w.svc.Check(ctx, &CheckRequest{
		Content:   text,
		Direction: DirectionResponse,
		Model:     w.model,
		APIKeyID:  w.apiKeyID,
		TeamID:    w.teamID,
		OrgID:     w.orgID,
		Prompt:    w.turnPrompt,
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

// FinalCheck runs the trailing window-mode check on leftover buffered text.
func (w *CallbackStreamGuardrail) FinalCheck(ctx context.Context, text string) (blocked bool, result *CheckResponse) {
	return w.CheckText(ctx, text)
}
