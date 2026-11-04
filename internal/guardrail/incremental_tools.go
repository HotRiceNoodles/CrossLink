package guardrail

import (
	"encoding/json"
	"errors"
	"strings"
)

// 只能替换 JSON 字符串内容。类型、键名和结构不得因遮盖而改变。
func maskJSONDocument(text string, spans []Span) (string, error) {
	if len(spans) == 0 {
		return text, nil
	}
	generic := false
	for _, sp := range spans {
		if sp.Start == 0 && sp.End == len(text) {
			generic = true
		}
	}
	if generic {
		var value any
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		if decoder.Decode(&value) != nil {
			return "", errors.New("invalid tool JSON")
		}
		var walk func(any) (any, error)
		walk = func(v any) (any, error) {
			switch x := v.(type) {
			case string:
				return "[REDACTED]", nil
			case map[string]any:
				// 整范围命中无法证明键名安全；保留原键会泄露，改键又会改变执行结构。
				return nil, errors.New("whole tool mask cannot preserve JSON keys safely")
			case []any:
				for i, item := range x {
					masked, err := walk(item)
					if err != nil {
						return nil, err
					}
					x[i] = masked
				}
				return x, nil
			default:
				return nil, errors.New("tool mask cannot preserve scalar type")
			}
		}
		value, err := walk(value)
		if err != nil {
			return "", err
		}
		out, _ := json.Marshal(value)
		return string(out), nil
	}
	masked := applySpans(text, spans)
	if !json.Valid([]byte(masked)) {
		return "", errors.New("tool mask cannot preserve JSON structure")
	}
	// 防止区间替换意外改变键名或数值类型。
	var original, replacement any
	d := json.NewDecoder(strings.NewReader(text))
	d.UseNumber()
	_ = d.Decode(&original)
	d = json.NewDecoder(strings.NewReader(masked))
	d.UseNumber()
	_ = d.Decode(&replacement)
	if !sameJSONStructure(original, replacement) {
		return "", errors.New("tool mask changes JSON structure")
	}
	return masked, nil
}

func sameJSONStructure(a, b any) bool {
	switch x := a.(type) {
	case string:
		_, ok := b.(string)
		return ok
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			item, exists := y[k]
			if !exists || !sameJSONStructure(v, item) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i, v := range x {
			if !sameJSONStructure(v, y[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

func (w *IncrementalSSEWriter) finalizeToolMasks() error {
	for key, spans := range w.stream.completeMasks {
		f := w.fields[key]
		if f == nil || !f.tool {
			continue
		}
		spans = mergeStreamSpans(append(append([]Span{}, f.maskSpans...), spans...))
		masked, err := maskJSONDocument(w.stream.completed[key], spans)
		if err != nil {
			return err
		}
		f.rewritten = &masked
	}
	for key, field := range w.fields {
		if !field.barrier {
			continue
		}
		text, err := w.stream.Render(key, 0, field.text.Len())
		if err != nil {
			return err
		}
		if text != field.text.String() {
			return errors.New("tool name cannot be masked safely")
		}
	}
	return nil
}
