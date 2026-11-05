package guardrail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

func ValidateRuleAction(kind, action string) error {
	if kind == "content_length" && action == "mask" {
		return errors.New("content_length 仅支持 block、log，不支持 mask")
	}
	return nil
}

func validStreamOffset(text string, n int) bool {
	return n >= 0 && n <= len(text) && (n == len(text) || utf8.RuneStart(text[n]))
}
func validStreamSpan(text string, sp Span) bool {
	return sp.Start < sp.End && validStreamOffset(text, sp.Start) && validStreamOffset(text, sp.End)
}

func safeStreamPreview(result *GuardrailResult) string {
	if result != nil {
		value := result.ContentSnippet
		if value == "[OUTPUT_LENGTH_EXCEEDED]" {
			return value
		}
		for _, span := range result.MaskedSpans {
			switch span.Replacement {
			case "[PHONE]", "[EMAIL]", "[ID_CARD]", "[ID]", "[CARD]", "[BANK_CARD]", "[IP]":
				return span.Replacement
			}
		}
		switch value {
		case "[PHONE]", "[EMAIL]", "[ID_CARD]", "[ID]", "[CARD]", "[BANK_CARD]", "[IP]":
			return value
		}
		if strings.HasPrefix(value, "[CREDENTIAL:") && strings.HasSuffix(value, "]") {
			safe := true
			for _, r := range value[len("[CREDENTIAL:") : len(value)-1] {
				if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
					safe = false
				}
			}
			if safe {
				return value
			}
		}
	}
	return "[REDACTED]"
}

// 引擎动作通过稳定 JSON 字段读取，兼容商业 overlay 的扩展结果类型。
func effectiveStreamAction(action string, result *GuardrailResult, logOnly bool) string {
	action = streamDecisionAction(action, result, logOnly)
	if action == "review" {
		return "block"
	}
	return action
}

func streamDecisionAction(action string, result *GuardrailResult, logOnly bool) string {
	if action == "" {
		action = "block"
	}
	if result != nil {
		raw, _ := json.Marshal(result)
		var value struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(raw, &value)
		switch value.Action {
		case "pass", "log", "block", "review":
			action = value.Action
		}
	}
	if logOnly && (action == "block" || action == "review") {
		action = "log"
	}
	return action
}

func streamBlock(sr *streamRule, result *GuardrailResult) *CheckResponse {
	action := streamDecisionAction(sr.rule.Action, result, false)
	recordBlock(sr.rule.Type, action)
	reason := "policy_match"
	return &CheckResponse{Blocked: true, Action: action, RuleName: sr.rule.Name, Reason: reason, Matched: []MatchedRule{{RuleID: sr.rule.ID, Version: sr.rule.Version, Action: action}}}
}

func streamEventPreview(sr *streamRule, result *GuardrailResult) string {
	if engine, ok := sr.engine.(interface{ StreamEventPreview(*GuardrailResult) string }); ok {
		return engine.StreamEventPreview(result)
	}
	return safeStreamPreview(result)
}

func mergeStreamSpans(spans []Span) []Span {
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].Start == spans[j].Start {
			return spans[i].End > spans[j].End
		}
		return spans[i].Start < spans[j].Start
	})
	var out []Span
	for _, sp := range spans {
		if len(out) > 0 && sp.Start < out[len(out)-1].End {
			last := &out[len(out)-1]
			if last.Start != sp.Start || last.End != sp.End || last.Replacement != sp.Replacement {
				last.Replacement = "[REDACTED]"
			}
			if sp.End > last.End {
				last.End = sp.End
			}
			continue
		}
		out = append(out, sp)
	}
	return out
}

// Render 只变换对外文本，原文坐标与所有引擎状态不受替换长度影响。
func (r *ResponseStream) Render(key string, start, end int) (string, error) {
	field := r.text[key]
	if field == nil {
		return "", errors.New("unknown incremental field")
	}
	text := field.String()
	if start > end || !validStreamOffset(text, start) || !validStreamOffset(text, end) {
		return "", errors.New("invalid incremental render range")
	}
	var b strings.Builder
	pos := start
	for _, sp := range r.masks[key] {
		if sp.End <= start || sp.Start >= end {
			continue
		}
		if sp.Start > pos {
			b.WriteString(text[pos:sp.Start])
		}
		if sp.Start >= start {
			b.WriteString(sp.Replacement)
		}
		if sp.End > pos {
			pos = sp.End
		}
		if pos >= end {
			return b.String(), nil
		}
	}
	if pos < end {
		b.WriteString(text[pos:end])
	}
	return b.String(), nil
}

func (r *ResponseStream) checkContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, CtxKeyCheck, &CheckContext{Metadata: map[string]string{"org_id": fmt.Sprint(r.meta.OrgID), "api_key_id": fmt.Sprint(r.meta.APIKeyID)}})
}

func (r *ResponseStream) checkComplete(ctx context.Context, text string, conservative bool) (*CheckResponse, error) {
	if !utf8.ValidString(text) || utf8.RuneCountInString(text) > r.svc.StrictMaxRunes() {
		return nil, errors.New("complete field exceeds inspection limit or invalid UTF-8")
	}
	var spans []Span
	var matched []MatchedRule
	ctx = r.checkContext(ctx)
	for i := range r.rules {
		sr := &r.rules[i]
		if sr.conservative != conservative || (!conservative && sr.rule.Type == "content_length") {
			continue
		}
		var result *GuardrailResult
		var err error
		if checker, ok := sr.engine.(interface {
			CheckStreamComplete(context.Context, string, Direction, string) (*GuardrailResult, error)
		}); ok {
			result, err = checker.CheckStreamComplete(ctx, text, DirectionResponse, r.meta.Model)
		} else {
			result, err = sr.engine.Check(ctx, text, DirectionResponse, r.meta.Model)
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			recordCheckError(sr.rule.Type, string(DirectionResponse))
			r.emitStreamError(ctx, sr, "engine_unavailable")
			if r.action(sr.rule) == "log" {
				continue
			}
			return nil, fmt.Errorf("incremental engine unavailable: %w", err)
		}
		if result == nil {
			return nil, errors.New("engine returned no inspection result")
		}
		incomplete := false
		if cc, _ := ctx.Value(CtxKeyCheck).(*CheckContext); cc != nil && cc.Metadata["guardrail_audit_status"] == "incomplete" {
			incomplete = true
			r.emitStreamError(ctx, sr, "engine_unavailable")
			delete(cc.Metadata, "guardrail_audit_status")
			delete(cc.Metadata, "guardrail_error_reason")
		}
		if !result.Blocked && len(result.MaskedSpans) == 0 {
			realMatch := false
			if engine, ok := sr.engine.(interface{ StreamResultMatched(*GuardrailResult) bool }); ok {
				realMatch = engine.StreamResultMatched(result)
			}
			if result.Reason != "" && (!incomplete || realMatch) {
				r.observe(ctx, sr, result)
			}
			continue
		}
		action := effectiveStreamAction(r.action(sr.rule), result, r.logOnly)
		if (!result.Blocked && action != "mask") || action == "pass" {
			r.observe(ctx, sr, result)
		} else {
			r.emit(ctx, sr, result)
		}
		matched = append(matched, MatchedRule{RuleID: sr.rule.ID, Version: sr.rule.Version, Action: streamDecisionAction(r.action(sr.rule), result, r.logOnly)})
		if action == "block" && result.Blocked {
			return streamBlock(sr, result), nil
		}
		if action != "mask" {
			continue
		}
		replacements := result.MaskedSpans
		if len(replacements) == 0 && len(text) > 0 {
			replacements = []Span{{Start: 0, End: len(text), Replacement: "[REDACTED]"}}
		}
		for _, sp := range replacements {
			if !validStreamSpan(text, sp) {
				return nil, errors.New("invalid complete replacement span")
			}
		}
		spans = append(spans, replacements...)
	}
	if len(spans) > 0 {
		spans = mergeStreamSpans(spans)
		return &CheckResponse{Action: "mask", MaskedSpans: spans, MaskedContent: applySpans(text, spans), Matched: matched}, nil
	}
	return nil, nil
}

func (r *ResponseStream) hasConservative() bool {
	for i := range r.rules {
		if r.ruleEnforces(&r.rules[i]) {
			return true
		}
	}
	return false
}

func (r *ResponseStream) ruleEnforces(sr *streamRule) bool {
	if !sr.conservative {
		return false
	}
	if engine, ok := sr.engine.(interface{ ResponseObservationOnly() bool }); ok && engine.ResponseObservationOnly() {
		return false
	}
	if r.action(sr.rule) != "log" {
		return true
	}
	// 分类引擎的有效动作可能覆盖规则行 log，判定前保留完整必要范围。
	return sr.rule.Type == "content_compliance" && !r.logOnly
}

func (r *ResponseStream) finishConservative(ctx context.Context) (*CheckResponse, error) {
	if r.finished {
		return nil, ctx.Err()
	}
	var texts []string
	for _, key := range r.order {
		text := r.text[key].String()
		if complete, ok := r.completed[key]; ok {
			text = complete
		}
		texts = append(texts, text)
	}
	text := strings.Join(texts, "\n")
	result, err := r.checkComplete(ctx, text, true)
	if err != nil || result != nil && result.Blocked {
		return result, err
	}
	if result != nil && result.Action == "mask" {
		offset := 0
		for _, key := range r.order {
			value := r.text[key].String()
			if complete, ok := r.completed[key]; ok {
				value = complete
			}
			end := offset + len(value)
			for _, sp := range result.MaskedSpans {
				start, stop := sp.Start, sp.End
				if start < offset {
					start = offset
				}
				if stop > end {
					stop = end
				}
				if start < stop {
					span := Span{Start: start - offset, End: stop - offset, Replacement: sp.Replacement}
					if _, ok := r.completed[key]; ok {
						if r.completeMasks == nil {
							r.completeMasks = map[string][]Span{}
						}
						r.completeMasks[key] = mergeStreamSpans(append(r.completeMasks[key], span))
					} else {
						r.masks[key] = mergeStreamSpans(append(r.masks[key], span))
					}
				}
			}
			offset = end + 1
		}
	}
	if result, err := r.finishAlternatives(ctx); err != nil || result != nil && result.Blocked {
		return result, err
	}
	for _, key := range r.order {
		r.released[key] = r.text[key].Len()
	}
	r.finished = true
	return nil, ctx.Err()
}
