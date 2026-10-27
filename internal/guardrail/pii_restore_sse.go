package guardrail

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// RestorePIIJSON rewrites content strings, not protocol identifiers, keys,
// signatures or usage. Parsing and re-encoding preserves JSON escaping.
func RestorePIIJSON(v any, vault *PIIVault) {
	switch x := v.(type) {
	case []any:
		for _, child := range x {
			RestorePIIJSON(child, vault)
		}
	case map[string]any:
		for key, child := range x {
			if text, ok := child.(string); ok {
				switch key {
				case "arguments":
					// Arguments are a second JSON document encoded inside a JSON
					// string. Decode it when complete, including Unicode escapes.
					if args, err := DecodePIIJSON([]byte(text)); err == nil {
						args = restorePIIValues(args, vault)
						encoded, _ := json.Marshal(args)
						x[key] = string(encoded)
					} else {
						x[key] = vault.Restore(text)
					}
				case "text", "content", "output_text", "reasoning_content", "refusal", "thinking", "output", "input":
					x[key] = vault.Restore(text)
				}
			} else if key == "input" || key == "arguments" {
				x[key] = restorePIIValues(child, vault)
			} else {
				RestorePIIJSON(child, vault)
			}
		}
	}
}

// Tool input objects have arbitrary user-defined keys (phone, recipient, ...).
// Restore string leaves in this payload without touching the surrounding IDs.
func restorePIIValues(value any, vault *PIIVault) any {
	switch x := value.(type) {
	case string:
		return vault.Restore(x)
	case []any:
		for i, child := range x {
			x[i] = restorePIIValues(child, vault)
		}
	case map[string]any:
		for key, child := range x {
			x[key] = restorePIIValues(child, vault)
		}
	}
	return value
}

func DecodePIIJSON(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, errors.New("invalid trailing response data")
	}
	return value, nil
}

type piiHeld struct {
	text  string
	owner map[string]any
	field string
	frame *piiFrame
}
type piiFrame struct {
	raw, prefix string
	obj         map[string]any
}

// PIIRestoreSSE retains only events containing possible split tokens and their
// successors. Choice/tool/block/item keys keep interleaved streams independent.
// It runs downstream of output guardrails, so restored PII is never re-scanned.
type PIIRestoreSSE struct {
	out                        io.Writer
	vault                      *PIIVault
	buffer                     string
	queue                      []*piiFrame
	held                       map[string]piiHeld
	items                      map[string]string
	bytes, maxBytes, maxEvents int
	err                        error
	current                    *piiFrame
}

func NewPIIRestoreSSE(out io.Writer, vault *PIIVault, maxBytes, maxEvents int) *PIIRestoreSSE {
	return &PIIRestoreSSE{out: out, vault: vault, held: map[string]piiHeld{}, items: map[string]string{}, maxBytes: maxBytes, maxEvents: maxEvents}
}

func (w *PIIRestoreSSE) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if len(w.buffer)+len(p)+w.bytes > w.maxBytes {
		w.err = errors.New("PII restore pending byte limit exceeded")
		return 0, w.err
	}
	w.buffer += string(p)
	for {
		end, n := strings.Index(w.buffer, "\n\n"), 2
		if cr := strings.Index(w.buffer, "\r\n\r\n"); cr >= 0 && (end < 0 || cr < end) {
			end, n = cr, 4
		}
		if end < 0 {
			break
		}
		raw := w.buffer[:end+n]
		w.buffer = w.buffer[end+n:]
		if err := w.frame(raw); err != nil {
			w.err = err
			return 0, err
		}
	}
	return len(p), nil
}

func (w *PIIRestoreSSE) delta(key string, obj map[string]any, field string) {
	text, ok := obj[field].(string)
	if !ok {
		return
	}
	previous := w.held[key]
	visible, tail := w.vault.RestorePrefix(previous.text+text, false)
	obj[field] = visible
	delete(w.held, key)
	if tail != "" {
		w.held[key] = piiHeld{text: tail, owner: obj, field: field, frame: w.current}
	}
}

func (w *PIIRestoreSSE) finish(prefix string) {
	for key, held := range w.held {
		if strings.HasPrefix(key, prefix) {
			held.owner[held.field] = str(held.owner[held.field]) + w.vault.Restore(held.text)
			delete(w.held, key)
		}
	}
}

func (w *PIIRestoreSSE) transform(obj map[string]any, typ string) {
	for _, entry := range array(obj["choices"]) {
		choice := object(entry)
		prefix := fmt.Sprint("c/", choice["index"], "/")
		delta := object(choice["delta"])
		for _, field := range []string{"content", "reasoning_content", "refusal"} {
			w.delta(prefix+field, delta, field)
		}
		for _, entry := range array(delta["tool_calls"]) {
			call := object(entry)
			w.delta(fmt.Sprint(prefix, "tool/", call["index"]), object(call["function"]), "arguments")
		}
		w.delta(prefix+"legacy", object(delta["function_call"]), "arguments")
		if str(choice["finish_reason"]) != "" {
			w.finish(prefix)
		}
	}
	prefix := fmt.Sprint("b/", obj["index"], "/")
	switch typ {
	case "content_block_start":
		block := object(obj["content_block"])
		for _, field := range []string{"text", "thinking"} {
			w.delta(prefix+field, block, field)
		}
	case "content_block_delta":
		delta := object(obj["delta"])
		for _, field := range []string{"text", "thinking", "partial_json"} {
			w.delta(prefix+field, delta, field)
		}
	case "content_block_stop":
		w.finish(prefix)
	}
	// Responses item_id is not present on every event; retain the index map.
	idx := fmt.Sprint(obj["output_index"])
	if item := object(obj["item"]); str(item["id"]) != "" {
		w.items[idx] = str(item["id"])
	}
	id := str(obj["item_id"])
	if id == "" {
		id = w.items[idx]
	}
	if id == "" {
		id = idx
	}
	if strings.HasPrefix(typ, "response.") && strings.HasSuffix(typ, ".delta") {
		key := fmt.Sprint("r/", id, "/", obj["content_index"], "/", obj["summary_index"], "/", strings.TrimSuffix(typ, ".delta"))
		w.delta(key, obj, "delta")
	}
	if typ == "response.output_item.done" {
		w.finish("r/" + id + "/")
	}
	if typ == "message_stop" || typ == "response.completed" || typ == "response.incomplete" || typ == "response.failed" || typ == "error" || obj["error"] != nil {
		w.finish("")
	}
	RestorePIIJSON(obj, w.vault)
}

func (w *PIIRestoreSSE) frame(raw string) error {
	if len(w.queue) >= w.maxEvents {
		return errors.New("PII restore pending event limit exceeded")
	}
	f := &piiFrame{raw: raw}
	w.current = f
	var data []string
	typ := ""
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		} else if line != "" {
			f.prefix += line + "\n"
			if strings.HasPrefix(line, "event:") {
				typ = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			}
		}
	}
	if len(data) > 0 {
		payload := strings.Join(data, "\n")
		if payload == "[DONE]" {
			w.finish("")
		} else {
			value, err := DecodePIIJSON([]byte(payload))
			if err != nil {
				return errors.New("invalid PII restore SSE payload")
			}
			obj, ok := value.(map[string]any)
			if !ok {
				return errors.New("invalid PII restore SSE object")
			}
			f.obj = obj
			if typ == "" {
				typ = str(obj["type"])
			}
			w.transform(obj, typ)
		}
	}
	w.queue = append(w.queue, f)
	w.bytes += len(raw)
	return w.release()
}

func (w *PIIRestoreSSE) release() error {
	// Only a frame that owns a partial token must wait; safe predecessors can
	// flush immediately. All successors preserve their original event order.
	for len(w.queue) > 0 {
		f := w.queue[0]
		blocked := false
		for _, held := range w.held {
			if held.frame == f {
				blocked = true
				break
			}
		}
		if blocked {
			break
		}
		output := f.raw
		if f.obj != nil {
			encoded, err := json.Marshal(f.obj)
			if err != nil {
				return err
			}
			output = f.prefix + "data: " + string(encoded) + "\n\n"
		}
		if _, err := io.WriteString(w.out, output); err != nil {
			return err
		}
		w.bytes -= len(f.raw)
		w.queue = w.queue[1:]
	}
	return nil
}

func (w *PIIRestoreSSE) Close() error {
	if w.err != nil {
		return w.err
	}
	// A disconnected/truncated event is never emitted as malformed JSON.
	if strings.TrimSpace(w.buffer) != "" {
		return errors.New("incomplete PII restore SSE event")
	}
	w.finish("")
	return w.release()
}
