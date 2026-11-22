package guardrail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

var ErrStreamBlocked = errors.New("stream blocked by guardrail")

type sseField struct {
	text                     strings.Builder
	release                  int
	tool, final, emptyObject bool
	rewritten                *string
	maskSpans                []Span
	barrier                  bool
}
type sseRequirement struct {
	key      string
	start    int
	end      int
	target   map[string]any
	property string
	snapshot bool
	value    *string
}
type pendingSSE struct {
	raw     string
	obj     map[string]any
	split   bool
	refs    []sseRequirement
	arrived time.Time
}

// IncrementalSSEWriter is a bounded, protocol-aware release queue. Generation
// and checking are pipelined by the provider's existing stream channel. Writes
// are serialized: no goroutine writes to Gin or runs a check per token in the
// background. Approved events and splittable text prefixes are released without
// sentence batching; tools and composite events retain their action barriers.
type IncrementalSSEWriter struct {
	ctx              context.Context
	stream           *ResponseStream
	out              io.Writer
	flush            func()
	buffer           string
	pending          []pendingSSE
	fields           map[string]*sseField
	items            map[int]string
	current          *pendingSSE
	bytes            int
	terminal, closed bool
	Result           *CheckResponse
	err              error
	protocol         string
	sequenceOffset   int64
	snapshotGroup    int
	replacements     map[string]string
	nativeBlock      bool
}

func NewIncrementalSSEWriter(ctx context.Context, stream *ResponseStream, out io.Writer, flush func()) *IncrementalSSEWriter {
	return &IncrementalSSEWriter{ctx: ctx, stream: stream, out: out, flush: flush, fields: map[string]*sseField{}, items: map[int]string{}}
}

func (w *IncrementalSSEWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	if len(w.buffer)+len(p)+w.bytes > w.stream.svc.StrictMaxBytes() {
		return 0, w.fail(errors.New("incremental pending byte limit exceeded"))
	}
	w.buffer += string(p)
	for {
		end, n := strings.Index(w.buffer, "\n\n"), 2
		if cr := strings.Index(w.buffer, "\r\n\r\n"); cr >= 0 && (end < 0 || cr < end) {
			end = cr
			n = 4
		}
		if end < 0 {
			break
		}
		raw := w.buffer[:end+n]
		w.buffer = w.buffer[end+n:]
		if err := w.frame(raw); err != nil {
			if errors.Is(err, ErrStreamBlocked) && w.current != nil && w.nativeBlock {
				_ = w.release()
				if len(w.pending) == 0 {
					_ = w.releaseTextPrefix(w.current)
				}
			}
			return 0, w.fail(err)
		}
	}
	return len(p), nil
}

func (w *IncrementalSSEWriter) frame(raw string) error {
	w.snapshotGroup++
	w.replacements = map[string]string{}
	w.nativeBlock = false
	if w.terminal {
		if strings.TrimSpace(raw) == "data: [DONE]" {
			_, err := io.WriteString(w.out, raw)
			if w.flush != nil {
				w.flush()
			}
			return err
		}
		return errors.New("data after terminal stream event")
	}
	if len(w.pending) >= w.stream.svc.StrictMaxEvents() {
		return errors.New("incremental pending event limit exceeded")
	}
	ev := pendingSSE{raw: raw, arrived: time.Now()}
	w.current = &ev
	var data []string
	typ := ""
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "data:") {
			v := strings.TrimPrefix(line, "data:")
			v = strings.TrimPrefix(v, " ")
			data = append(data, v)
		}
		if strings.HasPrefix(line, "event:") {
			typ = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
	}
	ctx, cancel := context.WithTimeout(w.ctx, time.Duration(w.stream.svc.ResponseBudgetMs())*time.Millisecond)
	defer cancel()
	terminal := false
	if len(data) > 0 {
		payload := strings.Join(data, "\n")
		if payload == "[DONE]" {
			terminal = true
		} else {
			var obj map[string]any
			decoder := json.NewDecoder(strings.NewReader(payload))
			decoder.UseNumber()
			if decoder.Decode(&obj) != nil || obj == nil {
				return errors.New("invalid SSE JSON payload")
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				return errors.New("invalid trailing SSE JSON payload")
			}
			ev.obj = obj
			if typ == "" {
				typ = str(obj["type"])
			}
			if _, ok := obj["choices"]; ok {
				w.protocol = "chat"
				if err := w.chat(ctx, obj); err != nil {
					return err
				}
			} else if strings.HasPrefix(typ, "response.") {
				w.protocol = "responses"
				if err := w.responses(ctx, typ, obj); err != nil {
					return err
				}
			} else if strings.HasPrefix(typ, "content_block_") || typ == "message_start" || typ == "message_delta" || typ == "message_stop" {
				w.protocol = "anthropic"
				if err := w.anthropic(ctx, typ, obj); err != nil {
					return err
				}
			} else if err := w.snapshot(ctx, obj); err != nil {
				return err
			}
			terminal = typ == "message_stop" || typ == "response.completed" || typ == "response.failed" || typ == "response.incomplete"
			if typ == "response.failed" || typ == "response.incomplete" {
				for _, field := range w.fields {
					if field.tool && !field.final {
						return errors.New("tool call was interrupted")
					}
				}
			}
		}
	}
	if terminal {
		if err := w.finishFields(ctx); err != nil {
			return err
		}
	}
	if len(w.replacements) > 0 {
		if result, err := w.stream.checkReplacements(ctx, w.replacements); err != nil {
			return err
		} else if result != nil && result.Blocked {
			w.Result = result
			return ErrStreamBlocked
		}
	}
	if terminal {
		if result, err := w.stream.Finish(ctx); err != nil {
			return err
		} else if result != nil && result.Blocked {
			w.Result = result
			return ErrStreamBlocked
		}
		for key, field := range w.fields {
			field.release = w.stream.released[key]
		}
		if err := w.finalizeToolMasks(); err != nil {
			return err
		}
	}
	w.pending = append(w.pending, ev)
	w.bytes += len(raw)
	w.current = nil
	if err := w.release(); err != nil {
		return err
	}
	if terminal {
		if len(w.pending) > 0 {
			return errors.New("terminal event has unapproved predecessors")
		}
		// Responses commonly sends [DONE] after response.completed. Keep
		// accepting that marker, but never accept more generated content.
		w.terminal = true
	}
	return nil
}

func (w *IncrementalSSEWriter) add(ctx context.Context, key, text string, tool bool) error {
	f := w.fields[key]
	if f == nil {
		if len(w.fields) >= w.stream.svc.StrictMaxEvents() {
			return errors.New("incremental field limit exceeded")
		}
		f = &sseField{tool: tool}
		w.fields[key] = f
	}
	if f.final && text != "" {
		return errors.New("content after field completion")
	}
	release, result, err := w.stream.Push(ctx, StreamFragment{Key: key, Text: text, Tool: tool})
	if err != nil {
		return err
	}
	f.text.WriteString(text)
	f.release = release
	w.current.refs = append(w.current.refs, sseRequirement{key: key, start: f.text.Len() - len(text), end: f.text.Len()})
	if result != nil && result.Blocked {
		w.Result = result
		w.nativeBlock = true
		return ErrStreamBlocked
	}
	return nil
}

// 纯文本 delta 可以在审核水位处拆分，避免同一帧末尾的候选前缀扣住安全前文。
// 工具、快照和复合事件仍以整帧为放行单位。
func (w *IncrementalSSEWriter) addText(ctx context.Context, key string, target map[string]any, property string) error {
	err := w.add(ctx, key, str(target[property]), false)
	if err != nil && !errors.Is(err, ErrStreamBlocked) {
		return err
	}
	ref := &w.current.refs[len(w.current.refs)-1]
	ref.target, ref.property = target, property
	return err
}

func (w *IncrementalSSEWriter) addBound(ctx context.Context, key, text string, tool bool, target map[string]any, property string) error {
	if property == "name" {
		tool = false
	}
	err := w.add(ctx, key, text, tool)
	if err != nil && !errors.Is(err, ErrStreamBlocked) {
		return err
	}
	ref := &w.current.refs[len(w.current.refs)-1]
	if property == "name" {
		w.fields[key].barrier = true
	}
	ref.target = target
	ref.property = property
	return err
}

func (w *IncrementalSSEWriter) complete(ctx context.Context, key string) error {
	f := w.fields[key]
	if f == nil || f.final {
		return nil
	}
	if f.tool {
		if f.text.Len() == 0 && f.emptyObject {
			if err := w.add(ctx, key, "{}", true); err != nil {
				return err
			}
		}
		var args any
		decoder := json.NewDecoder(strings.NewReader(f.text.String()))
		decoder.UseNumber()
		if decoder.Decode(&args) != nil {
			return errors.New("incomplete or invalid tool arguments")
		}
		canonical, _ := json.Marshal(args)
		result, err := w.stream.CheckComplete(ctx, string(canonical))
		if err != nil {
			return err
		} else if result != nil && result.Blocked {
			w.Result = result
			return ErrStreamBlocked
		}
		masked := string(canonical)
		if result != nil && result.Action == "mask" {
			f.maskSpans = result.MaskedSpans
			masked, err = maskJSONDocument(string(canonical), f.maskSpans)
			if err != nil {
				return err
			}
		}
		f.rewritten = &masked
		if w.stream.completed == nil {
			w.stream.completed = map[string]string{}
		}
		w.stream.completed[key] = string(canonical)
		w.replacements[key] = string(canonical)
	}
	release, result, err := w.stream.Push(ctx, StreamFragment{Key: key, Final: true})
	if err != nil {
		return err
	}
	f.release = release
	if result != nil && result.Blocked {
		w.Result = result
		w.nativeBlock = true
		return ErrStreamBlocked
	}
	if release != f.text.Len() && !w.stream.hasConservative() {
		return errors.New("incomplete final moderation coverage")
	}
	f.final = true
	f.release = release
	if strings.HasSuffix(key, "/name") {
		masked, err := w.stream.Render(key, 0, f.text.Len())
		if err != nil {
			return err
		}
		if masked != f.text.String() {
			return errors.New("tool name cannot be masked safely")
		}
	}
	return nil
}

func (w *IncrementalSSEWriter) completePrefix(ctx context.Context, prefix string) error {
	for key := range w.fields {
		if strings.HasPrefix(key, prefix) {
			if err := w.complete(ctx, key); err != nil {
				return err
			}
		}
	}
	return nil
}
func (w *IncrementalSSEWriter) finishFields(ctx context.Context) error {
	return w.completePrefix(ctx, "")
}

func (w *IncrementalSSEWriter) snapshot(ctx context.Context, v any) error {
	// Covers terminal-only content and future text-bearing event variants.
	// Structured data is decoded before scanning escaped JSON string leaves.
	var walk func(any) error
	walk = func(v any) error {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for key := range x {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				val := x[key]
				switch key {
				case "text", "content", "arguments", "thinking", "reasoning_content", "name", "refusal":
					if s, ok := val.(string); ok {
						field := fmt.Sprintf("snapshot%d/%s", len(w.fields), key)
						if err := w.addBound(ctx, field, s, key == "arguments", x, key); err != nil {
							return err
						}
						if err := w.complete(ctx, field); err != nil {
							return err
						}
					} else {
						if err := walk(val); err != nil {
							return err
						}
					}
				default:
					if err := walk(val); err != nil {
						return err
					}
				}
			}
		case []any:
			for _, val := range x {
				if err := walk(val); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(v)
}

func (w *IncrementalSSEWriter) release() error {
	for len(w.pending) > 0 {
		ev := &w.pending[0]
		ready := true
		for _, ref := range ev.refs {
			f := w.fields[ref.key]
			if f.release < ref.end || ((f.tool || f.barrier) && !f.final) {
				ready = false
				break
			}
		}
		if !ready {
			if err := w.releaseTextPrefix(ev); err != nil {
				return err
			}
			break
		}
		if _, err := io.WriteString(w.out, w.eventRaw(ev, false)); err != nil {
			return err
		}
		guardrailStreamReleaseSeconds.Observe(time.Since(ev.arrived).Seconds())
		w.bytes -= len(ev.raw)
		w.pending[0] = pendingSSE{}
		w.pending = w.pending[1:]
		if w.flush != nil {
			w.flush()
		}
	}
	return nil
}

func (w *IncrementalSSEWriter) releaseTextPrefix(ev *pendingSSE) error {
	if len(ev.refs) != 1 || ev.refs[0].target == nil || !splittableTextEvent(ev.obj) {
		return nil
	}
	ref := &ev.refs[0]
	f := w.fields[ref.key]
	if f.tool || f.release <= ref.start || f.release >= ref.end {
		return nil
	}
	end := ref.end
	ref.end = f.release
	raw := w.eventRaw(ev, true)
	ref.end = end
	if _, err := io.WriteString(w.out, raw); err != nil {
		return err
	}
	// Responses 的合成事件占用一个序号；后续事件统一平移，保持严格递增。
	if _, ok := ev.obj["sequence_number"].(json.Number); ok {
		w.sequenceOffset++
	}
	ref.start = f.release
	ref.target[ref.property] = f.text.String()[ref.start:ref.end]
	ev.split = true
	oldBytes := len(ev.raw)
	ev.raw = encodeSSEObject(ev.raw, ev.obj, false)
	w.bytes += len(ev.raw) - oldBytes
	if w.flush != nil {
		w.flush()
	}
	return nil
}

func (w *IncrementalSSEWriter) eventRaw(ev *pendingSSE, prefix bool) string {
	changed := false
	for _, ref := range ev.refs {
		if ref.target == nil {
			continue
		}
		text, err := w.stream.Render(ref.key, ref.start, ref.end)
		if ref.value != nil {
			text = *ref.value
			err = nil
		}
		if f := w.fields[ref.key]; f != nil && f.tool && f.rewritten != nil {
			if ref.property == "input" {
				if ref.end > 0 {
					var value any
					decoder := json.NewDecoder(strings.NewReader(*f.rewritten))
					decoder.UseNumber()
					_ = decoder.Decode(&value)
					ref.target[ref.property] = value
					changed = true
				}
				continue
			}
			text = ""
			if ref.start == 0 && ref.end > 0 || ref.snapshot {
				text = *f.rewritten
			}
			err = nil
		}
		if err != nil {
			continue
		} // 所有引用在入队时已校验边界。
		if str(ref.target[ref.property]) != text {
			changed = true
			ref.target[ref.property] = text
		}
	}
	if changed {
		stripOriginalTokenMetadata(ev.obj)
	}
	originalSequence, hasSequence := ev.obj["sequence_number"].(json.Number)
	if !prefix && !changed && !ev.split && (!hasSequence || w.sequenceOffset == 0) {
		return ev.raw
	}
	if hasSequence {
		sequence, _ := originalSequence.Int64()
		ev.obj["sequence_number"] = sequence + w.sequenceOffset
		defer func() { ev.obj["sequence_number"] = originalSequence }()
	}
	return encodeSSEObject(ev.raw, ev.obj, prefix)
}

func splittableTextEvent(obj map[string]any) bool {
	if obj["logprobs"] != nil || obj["usage"] != nil {
		return false
	}
	if choices, ok := obj["choices"]; ok {
		list := array(choices)
		if len(list) != 1 {
			return false
		}
		choice := object(list[0])
		return str(choice["finish_reason"]) == "" && choice["logprobs"] == nil
	}
	return true
}

func stripOriginalTokenMetadata(value any) {
	switch x := value.(type) {
	case map[string]any:
		delete(x, "logprobs")
		for _, v := range x {
			stripOriginalTokenMetadata(v)
		}
	case []any:
		for _, v := range x {
			stripOriginalTokenMetadata(v)
		}
	}
}

func encodeSSEObject(raw string, obj map[string]any, prefix bool) string {
	newline := "\n"
	if strings.Contains(raw, "\r\n") {
		newline = "\r\n"
	}
	payload, _ := json.Marshal(obj)
	lines := strings.Split(raw, newline)
	out := make([]string, 0, len(lines))
	written := false
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			if !written {
				out = append(out, "data: "+string(payload))
				written = true
			}
		} else if !prefix || !strings.HasPrefix(line, "id:") {
			out = append(out, line)
		}
	}
	return strings.Join(out, newline)
}

func (w *IncrementalSSEWriter) fail(err error) error {
	if w.err != nil {
		return w.err
	}
	w.err = err
	w.pending = nil
	w.buffer = ""
	w.current = nil
	w.bytes = 0
	reason := streamErrorReason(err, w.Result != nil && w.Result.RuleName != "guardrail_stream_error")
	if w.Result == nil {
		w.Result = &CheckResponse{Blocked: true, Action: "block", RuleName: "guardrail_stream_error", Reason: reason}
	}
	if w.ctx.Err() == nil {
		io.WriteString(w.out, streamFailureFrame(w.protocol, reason))
		if w.flush != nil {
			w.flush()
		}
	}
	return err
}

func (w *IncrementalSSEWriter) Close() error {
	if w.closed {
		return w.err
	}
	w.closed = true
	defer w.stream.Close()
	if w.err != nil {
		return w.err
	}
	// EOF is not a successful tool-call completion. Preserve already approved
	// text, drop unapproved tails and never release unfinished tool calls.
	if w.ctx.Err() != nil {
		w.pending = nil
		w.buffer = ""
		return w.ctx.Err()
	}
	if w.buffer != "" || len(w.pending) > 0 {
		return w.fail(errors.New("stream ended with unapproved content"))
	}
	if !w.terminal {
		ctx, cancel := context.WithTimeout(w.ctx, time.Duration(w.stream.svc.ResponseBudgetMs())*time.Millisecond)
		defer cancel()
		if _, err := w.stream.Finish(ctx); err != nil {
			return w.fail(err)
		}
	}
	return nil
}

func str(v any) string { s, _ := v.(string); return s }
func number(v any) int {
	if n, ok := v.(json.Number); ok {
		value, _ := n.Int64()
		return int(value)
	}
	n, _ := v.(float64)
	return int(n)
}
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func array(v any) []any           { a, _ := v.([]any); return a }

var _ io.Writer = (*IncrementalSSEWriter)(nil)
