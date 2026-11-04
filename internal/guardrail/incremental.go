package guardrail

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrStreamUnsupported requests the existing whole-response strict path. An
// engine must opt in explicitly; Check alone does not imply incremental safety.
var ErrStreamUnsupported = errors.New("incremental checking is unsupported")

// StreamFragment addresses one logical field, independently of SSE boundaries.
// Offsets in StreamVerdict are UTF-8 bytes in that field, not model tokens.
type StreamFragment struct {
	Key   string
	Text  string
	Final bool
	// Tool 参数暂留到完整 JSON 解码后计长；内容检测仍接收原始增量。
	Tool bool
}

type StreamVerdict struct {
	CheckedThrough int
	ReleaseThrough int
	Result         *GuardrailResult
	Spans          []Span
	BlockStart     *int
}

// StreamingEngine implementations create PRIVATE state for each response. The
// request includes the initial conversation text for contextual classifiers.
// A release position is a policy decision, not proof about future completions.
type StreamingEngine interface {
	NewStream(context.Context, CheckRequest) (StreamSession, error)
}

type StreamSession interface {
	Push(context.Context, StreamFragment) (StreamVerdict, error)
	Close()
}

// StreamReplacementSession 对替换已有字段的完成快照更新累计状态，避免
// 长度规则重复计数或只检查单字段而遗漏整条响应的总量。
type StreamReplacementSession interface {
	CheckReplacement(context.Context, string, string) (*GuardrailResult, error)
}

type streamRule struct {
	rule          GuardrailRule
	engine        GuardrailEngine
	session       StreamSession
	hit           bool
	observed      bool
	watermarks    map[string]int
	releaseEngine func()
	conservative  bool
}

// ResponseStream pins rule versions, model/tenant selection and engine state
// for one response. Calls are serialized by the stream writer; no token workers
// or mutable CheckContext are shared across responses.
type ResponseStream struct {
	svc           *GuardrailService
	meta          CheckRequest
	rules         []streamRule
	Fallback      bool
	text          map[string]*strings.Builder
	order         []string
	released      map[string]int
	runes         int
	closed        bool
	logOnly       bool
	masks         map[string][]Span
	finished      bool
	completed     map[string]string
	completeMasks map[string][]Span
	alternates    []*streamAlternative
}

func (s *GuardrailService) NewResponseStream(ctx context.Context, meta CheckRequest) (*ResponseStream, error) {
	if err := s.ensureRules(ctx); err != nil {
		return nil, err
	}
	r := &ResponseStream{svc: s, meta: meta, text: map[string]*strings.Builder{}, released: map[string]int{}, masks: map[string][]Span{}, logOnly: s.IsLogOnly()}
	mc := s.getModelConfig(ctx, meta.Model)
	if mc != nil && !mc.Enabled {
		return r, nil
	}
	s.mu.RLock()
	var rules []GuardrailRule
	orgOverride := false
	if meta.OrgID != 0 {
		for _, rule := range s.rules {
			if rule.Type == "content_compliance" && rule.OrgID != nil && *rule.OrgID == meta.OrgID {
				orgOverride = true
				break
			}
		}
	}
	for _, rule := range s.rules {
		if orgOverride && rule.Type == "content_compliance" && rule.OrgID == nil {
			continue
		}
		if !rule.Enabled || (rule.Direction != string(DirectionResponse) && rule.Direction != string(DirectionBoth)) ||
			(rule.OrgID != nil && *rule.OrgID != meta.OrgID) ||
			(rule.ModelFilter != nil && !modelInFilter(meta.Model, *rule.ModelFilter)) {
			continue
		}
		if mc != nil && len(mc.ResponseChecks) > 0 {
			found := false
			for _, kind := range mc.ResponseChecks {
				if kind == rule.Type {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		rules = append(rules, rule)
	}
	s.mu.RUnlock()
	sort.SliceStable(rules, func(i, j int) bool { return severityOrder[rules[i].Severity] > severityOrder[rules[j].Severity] })
	for _, rule := range rules {
		if err := ValidateRuleAction(rule.Type, rule.Action); err != nil {
			r.Close()
			return nil, err
		}
		eng, release, err := s.leaseStreamEngine(rule)
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("create streaming rule %s: %w", rule.Name, err)
		}
		if applicable, ok := eng.(interface{ SupportsDirection(Direction) bool }); ok && !applicable.SupportsDirection(DirectionResponse) {
			release()
			continue
		}
		if contextual, ok := eng.(interface{ WithTurnPrompt(string) GuardrailEngine }); ok {
			eng = contextual.WithTurnPrompt(meta.Prompt)
		}
		sr := streamRule{rule: rule, engine: eng, watermarks: map[string]int{}, releaseEngine: release}
		{
			if se, ok := eng.(StreamingEngine); ok {
				sr.session, err = se.NewStream(ctx, meta)
			} else {
				err = ErrStreamUnsupported
			}
			if errors.Is(err, ErrStreamUnsupported) {
				sr.conservative = true
			} else if err != nil {
				r.rules = append(r.rules, sr)
				r.Close()
				return nil, err
			} else if sr.session == nil {
				r.rules = append(r.rules, sr)
				r.Close()
				return nil, errors.New("streaming engine returned no session")
			}
		}
		r.rules = append(r.rules, sr)
	}
	return r, nil
}

func (r *ResponseStream) action(rule GuardrailRule) string {
	if r.logOnly && rule.Action != "mask" {
		return "log"
	}
	if rule.Action == "" {
		return "block"
	}
	return rule.Action
}

// Push returns the minimum contiguous release position across enforcing rules.
// A classifier cannot acknowledge bytes it has not received, regress its
// watermark, or leave a gap and still release a later event.
func (r *ResponseStream) Push(ctx context.Context, f StreamFragment) (int, *CheckResponse, error) {
	if r.closed || r.finished {
		return 0, nil, errors.New("stream already closed")
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if !utf8.ValidString(f.Text) {
		return 0, nil, errors.New("invalid UTF-8 stream fragment")
	}
	if _, ok := r.text[f.Key]; !ok {
		r.order = append(r.order, f.Key)
		r.text[f.Key] = &strings.Builder{}
	}
	r.runes += utf8.RuneCountInString(f.Text)
	if r.runes > r.svc.StrictMaxRunes() {
		return 0, nil, errors.New("incremental inspection limit exceeded")
	}
	r.text[f.Key].WriteString(f.Text)
	end := r.text[f.Key].Len()
	release := end
	var blocked *CheckResponse
	for i := range r.rules {
		sr := &r.rules[i]
		if sr.session == nil {
			if r.ruleEnforces(sr) {
				release = 0
			}
			continue
		}
		verdict, err := sr.session.Push(ctx, f)
		if err != nil {
			recordCheckError(sr.rule.Type, string(DirectionResponse))
			recordEngineError(sr.rule.Type)
			return 0, nil, err
		}
		recordCheck(sr.rule.Type, string(DirectionResponse), verdict.Result != nil && verdict.Result.Blocked)
		if verdict.CheckedThrough < verdict.ReleaseThrough || verdict.CheckedThrough > end ||
			verdict.ReleaseThrough < sr.watermarks[f.Key] || verdict.ReleaseThrough < 0 ||
			(verdict.ReleaseThrough < end && !utf8.RuneStart(r.text[f.Key].String()[verdict.ReleaseThrough])) {
			return 0, nil, errors.New("invalid incremental release watermark")
		}
		action := effectiveStreamAction(r.action(sr.rule), verdict.Result, r.logOnly)
		enforcing := action != "log" && action != "pass"
		if verdict.BlockStart != nil && ((enforcing && *verdict.BlockStart < r.released[f.Key]) || !validStreamOffset(r.text[f.Key].String(), *verdict.BlockStart)) {
			return 0, nil, errors.New("invalid incremental violation position")
		}
		for _, sp := range verdict.Spans {
			if (enforcing && sp.Start < r.released[f.Key]) || !validStreamSpan(r.text[f.Key].String(), sp) {
				return 0, nil, errors.New("invalid incremental replacement span")
			}
		}
		matched := verdict.Result != nil && verdict.Result.Blocked
		if matched || len(verdict.Spans) > 0 {
			if (!matched && action != "mask") || action == "pass" {
				r.observe(ctx, sr, verdict.Result)
			} else {
				r.emit(ctx, sr, verdict.Result)
			}
			if action == "block" && matched {
				start := sr.watermarks[f.Key]
				if verdict.BlockStart != nil {
					start = *verdict.BlockStart
				} else if len(verdict.Spans) > 0 {
					start = verdict.Spans[0].Start
				}
				if start < release {
					release = start
				}
				if blocked == nil {
					blocked = streamBlock(sr, verdict.Result)
				}
			} else if action == "mask" {
				if len(verdict.Spans) > 0 {
					r.masks[f.Key] = mergeStreamSpans(append(r.masks[f.Key], verdict.Spans...))
				} else if matched && verdict.BlockStart == nil {
					return 0, nil, errors.New("native mask hit has no replacement position")
				}
			}
		}
		sr.watermarks[f.Key] = verdict.ReleaseThrough
		if action != "log" && action != "pass" && verdict.ReleaseThrough < release {
			release = verdict.ReleaseThrough
		}
	}
	for _, span := range r.masks[f.Key] {
		if span.Start < release && release < span.End {
			release = span.Start
		}
	}
	r.released[f.Key] = release
	return release, blocked, nil
}

// CheckComplete is the action barrier for tool arguments and terminal snapshots.
// It uses the pinned rules and decoded JSON, including nonincremental log rules.
// Incremental mode never rewrites a partially emitted structured call: mask hits
// here are controlled refusals (mask policies normally select strict fallback).
func (r *ResponseStream) CheckComplete(ctx context.Context, text string) (*CheckResponse, error) {
	return r.checkComplete(ctx, text, false)
}

func (r *ResponseStream) CheckReplacement(ctx context.Context, key, text string) (*CheckResponse, error) {
	result, err := r.CheckComplete(ctx, text)
	if err != nil || result != nil && result.Blocked {
		return result, err
	}
	if blocked, err := r.checkReplacementState(ctx, key, text); err != nil || blocked != nil {
		return blocked, err
	}
	return result, nil
}

func (r *ResponseStream) checkReplacementState(ctx context.Context, key, text string) (*CheckResponse, error) {
	return r.checkReplacements(ctx, map[string]string{key: text})
}

func (r *ResponseStream) checkReplacements(ctx context.Context, texts map[string]string) (*CheckResponse, error) {
	for i := range r.rules {
		sr := &r.rules[i]
		checker, ok := sr.session.(StreamReplacementSession)
		if !ok {
			continue
		}
		var result *GuardrailResult
		var err error
		if batch, ok := sr.session.(interface {
			CheckReplacements(context.Context, map[string]string) (*GuardrailResult, error)
		}); ok {
			result, err = batch.CheckReplacements(ctx, texts)
		} else {
			keys := make([]string, 0, len(texts))
			for key := range texts {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				result, err = checker.CheckReplacement(ctx, key, texts[key])
				if err != nil || result != nil && result.Blocked {
					break
				}
			}
		}
		if err != nil {
			return nil, err
		}
		if result != nil && result.Blocked {
			r.emit(ctx, sr, result)
		}
		if result != nil && result.Blocked && r.action(sr.rule) == "block" {
			recordBlock(sr.rule.Type, "block")
			return &CheckResponse{Blocked: true, Action: "block", RuleName: sr.rule.Name, Reason: result.Reason,
				Matched: []MatchedRule{{RuleID: sr.rule.ID, Version: sr.rule.Version, Action: "block"}}}, nil
		}
	}
	return nil, nil
}

func (r *ResponseStream) Finish(ctx context.Context) (*CheckResponse, error) {
	return r.finishConservative(ctx)
}

func (r *ResponseStream) emit(ctx context.Context, sr *streamRule, result *GuardrailResult) {
	if sr.hit {
		return
	}
	sr.hit = true
	action := streamDecisionAction(r.action(sr.rule), result, r.logOnly)
	r.svc.emitEvent(ctx, GuardrailEvent{RuleID: sr.rule.ID, RuleVersion: sr.rule.Version, RuleName: sr.rule.Name,
		EngineType: sr.rule.Type, Direction: string(DirectionResponse), Severity: sr.rule.Severity, Action: action,
		Reason: "policy_match", ContentPreview: streamEventPreview(sr, result),
		APIKeyID: r.meta.APIKeyID, TeamID: r.meta.TeamID, OrgID: r.meta.OrgID, Model: r.meta.Model, Timestamp: time.Now()})
}

func (r *ResponseStream) Close() {
	if r.closed {
		return
	}
	r.closed = true
	for i := range r.rules {
		sr := &r.rules[i]
		if sr.session != nil {
			sr.session.Close()
		}
		if sr.releaseEngine != nil {
			sr.releaseEngine()
		}
	}
	r.text = nil
	r.released = nil
}

// A request without an installed SSE gate conservatively uses strict mode. This
// includes direct handler calls and paths that do not run request middleware.
const CtxKeyIncremental CtxKey = "guardrail_incremental_active"

func (s *GuardrailService) StreamModeFor(ctx context.Context) string {
	mode := s.StreamMode()
	if mode == "incremental" && (ctx == nil || ctx.Value(CtxKeyIncremental) != true) {
		return "strict"
	}
	return mode
}
