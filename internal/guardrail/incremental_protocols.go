package guardrail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func (w *IncrementalSSEWriter) chat(ctx context.Context, obj map[string]any) error {
	for _, value := range array(obj["choices"]) {
		choice := object(value)
		prefix := fmt.Sprintf("c%d/", number(choice["index"]))
		delta := object(choice["delta"])
		for _, field := range []string{"content", "reasoning_content", "refusal"} {
			if text := str(delta[field]); text != "" {
				if err := w.addText(ctx, prefix+field, delta, field); err != nil {
					return err
				}
			}
		}
		for _, value := range array(delta["tool_calls"]) {
			call := object(value)
			function := object(call["function"])
			key := fmt.Sprintf("%st%d/", prefix, number(call["index"]))
			if err := w.addBound(ctx, key+"args", str(function["arguments"]), true, function, "arguments"); err != nil {
				return err
			}
			if name := str(function["name"]); name != "" {
				if err := w.addBound(ctx, key+"name", name, true, function, "name"); err != nil {
					return err
				}
			}
		}
		// Legacy function_call is still accepted by some compatible providers.
		if call := object(delta["function_call"]); call != nil {
			if err := w.addBound(ctx, prefix+"legacy/args", str(call["arguments"]), true, call, "arguments"); err != nil {
				return err
			}
			if name := str(call["name"]); name != "" {
				if err := w.addBound(ctx, prefix+"legacy/name", name, true, call, "name"); err != nil {
					return err
				}
			}
		}
		if reason := str(choice["finish_reason"]); reason != "" {
			if reason == "length" || reason == "content_filter" {
				for key, f := range w.fields {
					if strings.HasPrefix(key, prefix) && f.tool && !f.final {
						return errors.New("tool call was interrupted")
					}
				}
			}
			if err := w.completePrefix(ctx, prefix); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *IncrementalSSEWriter) anthropic(ctx context.Context, typ string, obj map[string]any) error {
	prefix := fmt.Sprintf("b%d/", number(obj["index"]))
	switch typ {
	case "content_block_start":
		block := object(obj["content_block"])
		switch str(block["type"]) {
		case "text":
			return w.addText(ctx, prefix+"text", block, "text")
		case "thinking":
			return w.addText(ctx, prefix+"thinking", block, "thinking")
		case "tool_use":
			args := ""
			if input := block["input"]; input != nil {
				raw, _ := json.Marshal(input)
				if string(raw) != "{}" {
					args = string(raw)
				}
			}
			if err := w.addBound(ctx, prefix+"args", args, true, block, "input"); err != nil {
				return err
			}
			if args == "" {
				w.fields[prefix+"args"].emptyObject = true
			}
			return w.addBound(ctx, prefix+"name", str(block["name"]), true, block, "name")
		}
	case "content_block_delta":
		delta := object(obj["delta"])
		switch str(delta["type"]) {
		case "text_delta":
			return w.addText(ctx, prefix+"text", delta, "text")
		case "thinking_delta":
			return w.addText(ctx, prefix+"thinking", delta, "thinking")
		case "input_json_delta":
			return w.addBound(ctx, prefix+"args", str(delta["partial_json"]), true, delta, "partial_json")
		}
	case "content_block_stop":
		return w.completePrefix(ctx, prefix)
	}
	return w.snapshot(ctx, obj)
}

func (w *IncrementalSSEWriter) responseKey(obj map[string]any) string {
	id := str(obj["item_id"])
	idx := number(obj["output_index"])
	if id == "" {
		id = w.items[idx]
	}
	if id == "" {
		id = fmt.Sprintf("index%d", idx)
	}
	return "r/" + id + "/"
}

func (w *IncrementalSSEWriter) responseTextKey(typ string, obj map[string]any) string {
	prefix := w.responseKey(obj)
	if strings.Contains(typ, "summary_text") {
		return fmt.Sprintf("%ssummary%d", prefix, number(obj["summary_index"]))
	}
	field := "text"
	if strings.Contains(typ, "reasoning_text") {
		field = "reasoning"
	} else if strings.Contains(typ, "refusal") {
		field = "refusal"
	}
	return fmt.Sprintf("%scontent%d/%s", prefix, number(obj["content_index"]), field)
}

func (w *IncrementalSSEWriter) responseSnapshot(ctx context.Context, key, text string, tool bool) error {
	f := w.fields[key]
	if f == nil {
		if err := w.add(ctx, key, text, tool); err != nil {
			return err
		}
	} else if f.text.String() != text {
		// Snapshots are alternate complete payloads, not deltas. A tool-call
		// snapshot must agree with the arguments the client accumulated.
		if tool {
			return errors.New("tool argument snapshot disagrees with deltas")
		}
		if result, err := w.stream.CheckComplete(ctx, text); err != nil {
			return err
		} else if result != nil && result.Blocked {
			w.Result = result
			return ErrStreamBlocked
		}
	}
	// 即使快照又变回原始 delta 文本，也要撤销先前替换的计数差额。
	// 工具字段由 complete 中的完整 JSON 规范化结果计数。
	if !tool {
		w.replacements[key] = text
	}
	return w.complete(ctx, key)
}

func (w *IncrementalSSEWriter) responseSnapshotBound(ctx context.Context, key string, target map[string]any, property string, tool bool) error {
	text := str(target[property])
	if field := w.fields[key]; property == "name" && field != nil && field.text.String() != text {
		return errors.New("tool name snapshot disagrees with deltas")
	}
	if err := w.responseSnapshot(ctx, key, text, tool); err != nil {
		return err
	}
	f := w.fields[key]
	if property == "name" {
		f.barrier = true
	}
	ref := sseRequirement{key: key, start: 0, end: f.text.Len(), target: target, property: property, snapshot: true}
	changed := text != f.text.String()
	for _, previous := range w.stream.alternates {
		if previous.key == key {
			changed = true
			break
		}
	}
	if changed {
		value, err := w.stream.alternative(ctx, key, text, w.snapshotGroup)
		if err != nil {
			return err
		}
		ref.value = value
	}
	w.current.refs = append(w.current.refs, ref)
	return nil
}

func (w *IncrementalSSEWriter) responseItem(ctx context.Context, item map[string]any, idx int, done bool) error {
	id := str(item["id"])
	if id != "" {
		w.items[idx] = id
	}
	obj := map[string]any{"item_id": id, "output_index": float64(idx)}
	prefix := w.responseKey(obj)
	if str(item["type"]) == "function_call" {
		args := str(item["arguments"])
		if done {
			if err := w.responseSnapshotBound(ctx, prefix+"args", item, "arguments", true); err != nil {
				return err
			}
		} else {
			if err := w.addBound(ctx, prefix+"args", args, true, item, "arguments"); err != nil {
				return err
			}
		}
		name := str(item["name"])
		if done {
			if err := w.responseSnapshotBound(ctx, prefix+"name", item, "name", false); err != nil {
				return err
			}
		} else {
			if err := w.addBound(ctx, prefix+"name", name, false, item, "name"); err != nil {
				return err
			}
		}
	}
	for i, value := range array(item["content"]) {
		part := object(value)
		field := "text"
		if strings.Contains(str(part["type"]), "reasoning") {
			field = "reasoning"
		} else if str(part["type"]) == "refusal" {
			field = "refusal"
		}
		key := fmt.Sprintf("%scontent%d/%s", prefix, i, field)
		text := str(part["text"])
		property := "text"
		if field == "refusal" {
			text = str(part["refusal"])
			property = "refusal"
		}
		if done {
			if err := w.responseSnapshotBound(ctx, key, part, property, false); err != nil {
				return err
			}
		} else if text != "" {
			if err := w.addText(ctx, key, part, property); err != nil {
				return err
			}
		}
	}
	for i, value := range array(item["summary"]) {
		text := str(object(value)["text"])
		key := fmt.Sprintf("%ssummary%d", prefix, i)
		if done {
			if err := w.responseSnapshotBound(ctx, key, object(value), "text", false); err != nil {
				return err
			}
		} else if text != "" {
			if err := w.addText(ctx, key, object(value), "text"); err != nil {
				return err
			}
		}
	}
	if done {
		return w.completePrefix(ctx, prefix)
	}
	return nil
}

func (w *IncrementalSSEWriter) responses(ctx context.Context, typ string, obj map[string]any) error {
	switch typ {
	case "response.output_item.added", "response.output_item.done":
		return w.responseItem(ctx, object(obj["item"]), number(obj["output_index"]), strings.HasSuffix(typ, ".done"))
	case "response.function_call_arguments.delta":
		return w.addBound(ctx, w.responseKey(obj)+"args", str(obj["delta"]), true, obj, "delta")
	case "response.function_call_arguments.done":
		return w.responseSnapshotBound(ctx, w.responseKey(obj)+"args", obj, "arguments", true)
	case "response.output_text.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.refusal.delta":
		return w.addText(ctx, w.responseTextKey(typ, obj), obj, "delta")
	case "response.output_text.done", "response.reasoning_text.done", "response.reasoning_summary_text.done", "response.refusal.done":
		property := "text"
		if typ == "response.refusal.done" {
			property = "refusal"
		}
		return w.responseSnapshotBound(ctx, w.responseTextKey(typ, obj), obj, property, false)
	case "response.content_part.added", "response.content_part.done":
		part := object(obj["part"])
		text := str(part["text"])
		property := "text"
		textType := "response.output_text"
		if strings.Contains(str(part["type"]), "reasoning") {
			textType = "response.reasoning_text"
		} else if str(part["type"]) == "refusal" {
			textType = "response.refusal"
			text = str(part["refusal"])
			property = "refusal"
		}
		key := w.responseTextKey(textType, obj)
		if strings.HasSuffix(typ, ".done") {
			return w.responseSnapshotBound(ctx, key, part, property, false)
		}
		if text != "" {
			return w.addText(ctx, key, part, property)
		}
	case "response.completed":
		for i, value := range array(object(obj["response"])["output"]) {
			if err := w.responseItem(ctx, object(value), i, true); err != nil {
				return err
			}
		}
	default:
		if strings.HasSuffix(typ, ".delta") && str(obj["delta"]) != "" {
			return errors.New("unsupported incremental delta event")
		}
		return w.snapshot(ctx, obj)
	}
	return nil
}
