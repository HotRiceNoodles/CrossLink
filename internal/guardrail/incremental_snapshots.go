package guardrail

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

type streamAlternative struct {
	key, text, masked string
	spans             []Span
	group             int
}

func (r *ResponseStream) alternative(ctx context.Context, key, text string, group int) (*string, error) {
	for _, alt := range r.alternates {
		if alt.key == key && alt.text == text && alt.group == group {
			return &alt.masked, nil
		}
	}
	if len(r.alternates) >= r.svc.StrictMaxEvents() || r.runes+utf8.RuneCountInString(text) > r.svc.StrictMaxRunes() {
		return nil, errors.New("snapshot inspection limit exceeded")
	}
	result, err := r.CheckComplete(ctx, text)
	if err != nil {
		return nil, err
	}
	if result != nil && result.Blocked {
		return nil, ErrStreamBlocked
	}
	alt := &streamAlternative{key: key, text: text, masked: text, group: group}
	if result != nil {
		alt.spans = result.MaskedSpans
		alt.masked = result.MaskedContent
	}
	r.alternates = append(r.alternates, alt)
	r.runes += utf8.RuneCountInString(text)
	return &alt.masked, nil
}

func (r *ResponseStream) finishAlternatives(ctx context.Context) (*CheckResponse, error) {
	latest := map[string]*streamAlternative{}
	for position := 0; position < len(r.alternates); {
		group := r.alternates[position].group
		for position < len(r.alternates) && r.alternates[position].group == group {
			alt := r.alternates[position]
			latest[alt.key] = alt
			position++
		}
		var texts []string
		for _, key := range r.order {
			text := r.text[key].String()
			if complete, ok := r.completed[key]; ok {
				text = complete
			}
			if alt := latest[key]; alt != nil {
				text = alt.text
			}
			texts = append(texts, text)
		}
		result, err := r.checkComplete(ctx, strings.Join(texts, "\n"), true)
		if err != nil || result != nil && result.Blocked {
			return result, err
		}
		if result == nil {
			continue
		}
		offset := 0
		for i, key := range r.order {
			end := offset + len(texts[i])
			for _, sp := range result.MaskedSpans {
				start, stop := sp.Start, sp.End
				if start < offset {
					start = offset
				}
				if stop > end {
					stop = end
				}
				if start >= stop {
					continue
				}
				mapped := Span{Start: start - offset, End: stop - offset, Replacement: sp.Replacement}
				if alt := latest[key]; alt != nil {
					alt.spans = append(alt.spans, mapped)
				} else if _, ok := r.completed[key]; ok {
					if r.completeMasks == nil {
						r.completeMasks = map[string][]Span{}
					}
					r.completeMasks[key] = mergeStreamSpans(append(r.completeMasks[key], mapped))
				} else {
					r.masks[key] = mergeStreamSpans(append(r.masks[key], mapped))
				}
			}
			offset = end + 1
		}
	}
	for _, alt := range r.alternates {
		alt.masked = applySpans(alt.text, mergeStreamSpans(alt.spans))
	}
	return nil, nil
}

func (r *ResponseStream) emitStreamError(ctx context.Context, sr *streamRule, reason string) {
	r.svc.emitEvent(ctx, GuardrailEvent{RuleID: sr.rule.ID, RuleVersion: sr.rule.Version, RuleName: sr.rule.Name, EngineType: sr.rule.Type, Direction: string(DirectionResponse), Severity: sr.rule.Severity, Action: "error", Reason: reason, ContentPreview: "[REDACTED]", Model: r.meta.Model, APIKeyID: r.meta.APIKeyID, TeamID: r.meta.TeamID, OrgID: r.meta.OrgID, Timestamp: time.Now()})
}

func (r *ResponseStream) observe(ctx context.Context, sr *streamRule, result *GuardrailResult) {
	if sr.observed {
		return
	}
	sr.observed = true
	r.svc.emitEvent(ctx, GuardrailEvent{RuleID: sr.rule.ID, RuleVersion: sr.rule.Version, RuleName: sr.rule.Name, EngineType: sr.rule.Type, Direction: string(DirectionResponse), Severity: sr.rule.Severity, Action: "log", Reason: "observation", ContentPreview: streamEventPreview(sr, result), Model: r.meta.Model, APIKeyID: r.meta.APIKeyID, TeamID: r.meta.TeamID, OrgID: r.meta.OrgID, Timestamp: time.Now()})
}
