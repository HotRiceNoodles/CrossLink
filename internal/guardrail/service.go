package guardrail

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"
)

var severityOrder = map[string]int{
	"critical": 4,
	"high":     3,
	"medium":   2,
	"low":      1,
}

type GuardrailService struct {
	db       *gorm.DB
	deps     EngineDeps
	enabled  atomic.Bool
	logOnly  atomic.Bool
	failOpen atomic.Bool

	// Detection execution policy (system_settings, hot-reloaded):
	//   streamMode          "window" (legacy sliding window, chunks may be
	//                       emitted before the covering check runs) or
	//                       "strict" (full buffering, nothing is emitted
	//                       before the final check passes).
	//   strictMaxRunes      strict-mode buffer cap; exceeding it fails closed.
	//   requestBudgetMs     total request-direction detection budget.
	//   responseBudgetMs    budget for one response/stream final check.
	//   oversizeAction      "truncate" (legacy, flagged as partial) or
	//                       "reject" (fail closed, never silently partial).
	//   engineErrorAction   "skip" (legacy: low-severity engine errors are
	//                       skipped, observable via error events) or "block".
	//   strictCoverage      reject uninspectable content endpoints instead of
	//                       passing them through unchecked.
	streamMode        atomic.Value // string
	strictMaxRunes    atomic.Int64
	strictMaxBytes    atomic.Int64
	strictMaxEvents   atomic.Int64
	requestBudgetMs   atomic.Int64
	responseBudgetMs  atomic.Int64
	oversizeAction    atomic.Value // string
	engineErrorAction atomic.Value // string
	strictCoverage    atomic.Bool

	mu       sync.RWMutex
	rules    []GuardrailRule
	loadedAt time.Time
	ttl      time.Duration

	sf singleflight.Group

	engineMu sync.Mutex
	engines  map[int64]GuardrailEngine

	handlersMu sync.RWMutex
	handlers   []EventHandler

	modelCfgMu sync.RWMutex
	modelCfgs  map[string]modelCfgEntry
}

type modelCfgEntry struct {
	config ModelGuardrailConfig
	loaded time.Time
}

// Detection policy defaults. All overridable via system_settings (see
// RefreshSettings); defaults keep pre-2026-10 behavior (window streaming,
// truncation, engine-error skip) so existing deployments are unchanged until
// an admin opts into the stricter modes.
const (
	DefaultStreamMode        = "window"
	DefaultStrictMaxRunes    = 100000
	DefaultStrictMaxBytes    = 4 << 20 // 4 MiB of buffered raw stream data
	DefaultStrictMaxEvents   = 100000  // buffered SSE chunks/events
	DefaultRequestBudgetMs   = 2000
	DefaultResponseBudgetMs  = 5000
	DefaultOversizeAction    = "truncate"
	DefaultEngineErrorAction = "skip"
)

func NewGuardrailService(db *gorm.DB, rdb redis.Cmdable) *GuardrailService {
	s := &GuardrailService{
		db:        db,
		deps:      EngineDeps{Redis: rdb, DB: db},
		ttl:       30 * time.Second,
		engines:   make(map[int64]GuardrailEngine),
		modelCfgs: make(map[string]modelCfgEntry),
		failOpen:  atomic.Bool{}, // default false: DB failure = error
	}
	s.streamMode.Store(DefaultStreamMode)
	s.strictMaxRunes.Store(DefaultStrictMaxRunes)
	s.strictMaxBytes.Store(DefaultStrictMaxBytes)
	s.strictMaxEvents.Store(DefaultStrictMaxEvents)
	s.requestBudgetMs.Store(DefaultRequestBudgetMs)
	s.responseBudgetMs.Store(DefaultResponseBudgetMs)
	s.oversizeAction.Store(DefaultOversizeAction)
	s.engineErrorAction.Store(DefaultEngineErrorAction)
	return s
}

func (s *GuardrailService) SetFailOpen(v bool) { s.failOpen.Store(v) }
func (s *GuardrailService) IsFailOpen() bool   { return s.failOpen.Load() }

func (s *GuardrailService) StreamMode() string {
	if v, ok := s.streamMode.Load().(string); ok && v != "" {
		return v
	}
	return DefaultStreamMode
}
func (s *GuardrailService) StrictMaxRunes() int {
	if v := s.strictMaxRunes.Load(); v > 0 {
		return int(v)
	}
	return DefaultStrictMaxRunes
}
func (s *GuardrailService) StrictMaxBytes() int {
	if v := s.strictMaxBytes.Load(); v > 0 {
		return int(v)
	}
	return DefaultStrictMaxBytes
}
func (s *GuardrailService) StrictMaxEvents() int {
	if v := s.strictMaxEvents.Load(); v > 0 {
		return int(v)
	}
	return DefaultStrictMaxEvents
}
func (s *GuardrailService) RequestBudgetMs() int {
	if v := s.requestBudgetMs.Load(); v > 0 {
		return int(v)
	}
	return DefaultRequestBudgetMs
}
func (s *GuardrailService) ResponseBudgetMs() int {
	if v := s.responseBudgetMs.Load(); v > 0 {
		return int(v)
	}
	return DefaultResponseBudgetMs
}
func (s *GuardrailService) OversizeAction() string {
	if v, ok := s.oversizeAction.Load().(string); ok && v != "" {
		return v
	}
	return DefaultOversizeAction
}
func (s *GuardrailService) EngineErrorAction() string {
	if v, ok := s.engineErrorAction.Load().(string); ok && v != "" {
		return v
	}
	return DefaultEngineErrorAction
}
func (s *GuardrailService) StrictCoverage() bool { return s.strictCoverage.Load() }

func (s *GuardrailService) SetIsDebugEnabled(fn func() bool)     { s.deps.IsDebugEnabled = fn }
func (s *GuardrailService) SetCheckLicenseTier(fn func() string) { s.deps.CheckLicenseTier = fn }
func (s *GuardrailService) SetRedisClient(c *redis.Client)       { s.deps.RedisClient = c }

func (s *GuardrailService) AddHandler(h EventHandler) {
	s.handlersMu.Lock()
	s.handlers = append(s.handlers, h)
	s.handlersMu.Unlock()
}

func (s *GuardrailService) emitEvent(ctx context.Context, event GuardrailEvent) {
	s.handlersMu.RLock()
	handlers := s.handlers
	s.handlersMu.RUnlock()
	for _, h := range handlers {
		go func(h EventHandler) {
			start := time.Now()
			defer func() {
				if r := recover(); r != nil {
					recordEventDrop("panic")
					slog.Error("guardrail: handler panic", "error", r)
				}
			}()
			timeoutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			h(timeoutCtx, event)
			// Drop semantics (mirrored in the metric help): dispatch did not
			// finish within its budget. This does NOT prove the handler's
			// write failed — it may have persisted before the deadline.
			if elapsed := time.Since(start); elapsed >= 5*time.Second {
				recordEventDrop("timeout")
			}
		}(h)
	}
}
func (s *GuardrailService) IsEnabled() bool   { return s.enabled.Load() }
func (s *GuardrailService) SetEnabled(v bool) { s.enabled.Store(v) }

// SetStreamMode overrides the runtime stream mode ("window"/"strict");
// production reads it from system_settings via RefreshSettings.
func (s *GuardrailService) SetStreamMode(v string) { s.streamMode.Store(v) }

func (s *GuardrailService) IsLogOnly() bool   { return s.logOnly.Load() }
func (s *GuardrailService) SetLogOnly(v bool) { s.logOnly.Store(v) }

func (s *GuardrailService) ensureRules(ctx context.Context) error {
	s.mu.RLock()
	fresh := time.Since(s.loadedAt) < s.ttl
	hasExisting := len(s.rules) > 0
	s.mu.RUnlock()
	if fresh {
		return nil
	}
	s.RefreshSettings(ctx)
	_, err, _ := s.sf.Do("loadRules", func() (any, error) {
		return nil, s.LoadRules(ctx)
	})
	if err != nil && hasExisting {
		slog.Warn("guardrail: using stale rules after DB load failure", "error", err)
		return nil
	}
	return err
}

func (s *GuardrailService) LoadRules(ctx context.Context) error {
	var rules []GuardrailRule
	if err := s.db.WithContext(ctx).Where("enabled = ?", true).Order("id ASC").Find(&rules).Error; err != nil {
		return err
	}

	s.mu.Lock()
	oldRules := make(map[int64]GuardrailRule, len(s.rules))
	for _, r := range s.rules {
		oldRules[r.ID] = r
	}
	s.rules = rules
	s.loadedAt = time.Now()
	s.mu.Unlock()
	recordRulesLoadedSuccess()

	// Evict removed and stale engines in a single lock section
	s.engineMu.Lock()
	activeIDs := make(map[int64]bool, len(rules))
	for _, r := range rules {
		activeIDs[r.ID] = true
	}
	for id, eng := range s.engines {
		if !activeIDs[id] {
			if c, ok := eng.(EngineCloser); ok {
				c.Close()
			}
			delete(s.engines, id)
		}
	}
	for _, r := range rules {
		if old, ok := oldRules[r.ID]; ok && string(old.Config) != string(r.Config) {
			if eng, exists := s.engines[r.ID]; exists {
				if c, ok := eng.(EngineCloser); ok {
					c.Close()
				}
			}
			delete(s.engines, r.ID)
		}
	}
	s.engineMu.Unlock()

	return nil
}

func (s *GuardrailService) getEngine(rule GuardrailRule) (GuardrailEngine, error) {
	s.engineMu.Lock()
	defer s.engineMu.Unlock()

	if eng, ok := s.engines[rule.ID]; ok {
		return eng, nil
	}

	eng, err := CreateEngine(rule.Type, json.RawMessage(rule.Config), s.deps)
	if err != nil {
		return nil, err
	}
	s.engines[rule.ID] = eng
	return eng, nil
}

func (s *GuardrailService) InvalidateCache() {
	s.mu.Lock()
	s.loadedAt = time.Time{}
	s.mu.Unlock()
	s.modelCfgMu.Lock()
	s.modelCfgs = make(map[string]modelCfgEntry)
	s.modelCfgMu.Unlock()
}

// channelRulesReload is the Redis Pub/Sub channel used to propagate rule and
// guardrail-config changes to every instance. Payload is a monotonic nonce
// (unix-nano) so identical consecutive changes still produce distinct
// messages.
const channelRulesReload = "guardrail:rules:reload"

// NotifyRulesChanged invalidates THIS instance's rule/settings caches
// immediately and then best-effort broadcasts a reload signal to other
// instances. Callers must invoke it only AFTER the change has committed
// successfully — never on a failed transaction (a failure must not produce
// an "effective" signal).
//
// Propagation honesty: this is neither atomic nor guaranteed — a lost Redis
// message leaves other instances on stale rules until their bounded reload
// TTL (30s default) refreshes them; staleness is observable via
// cl_guardrail_rules_last_loaded_success_timestamp_seconds. Publish failures
// never fail the caller's request.
func (s *GuardrailService) NotifyRulesChanged(ctx context.Context) {
	s.InvalidateCache()
	client := s.deps.RedisClient
	if client == nil {
		slog.Warn("guardrail: rules changed without a full redis client — other instances rely on the TTL reload only")
		return
	}
	if err := client.Publish(ctx, channelRulesReload, time.Now().UnixNano()).Err(); err != nil {
		// The local instance is already consistent; cross-instance
		// convergence falls back to the bounded TTL. Visible in logs, never
		// propagated to the caller.
		slog.Warn("guardrail: rules reload broadcast failed; other instances fall back to the TTL reload", "error", err)
	}
}

// StartReloadSubscription subscribes to rules-change broadcasts for the
// lifetime of ctx (cancel ctx to stop). On each message the local caches are
// invalidated and settings are refreshed immediately, so even an idle
// instance converges; rules themselves reload lazily on the next check (the
// existing ensureRules pattern). Connection loss is handled by the redis
// client's internal reconnect; regardless of Pub/Sub health, the bounded
// reload TTL caps staleness. No notifications are sent from this path — the
// subscription only invalidates local caches.
func (s *GuardrailService) StartReloadSubscription(ctx context.Context) {
	client := s.deps.RedisClient
	if client == nil {
		slog.Info("guardrail: rules reload subscription disabled (no full redis client); cross-instance propagation relies on the TTL reload only")
		return
	}
	go func() {
		sub := client.Subscribe(ctx, channelRulesReload)
		defer sub.Close()
		ch := sub.Channel()
		slog.Info("guardrail: subscribed to rules reload broadcasts", "channel", channelRulesReload)
		for {
			select {
			case <-ch:
				s.InvalidateCache()
				// Refresh settings right away so global switches converge on
				// an idle instance too; rules reload lazily on next check.
				s.RefreshSettings(context.Background())
			case <-ctx.Done():
				return
			}
		}
	}()
}

// SetRulesTTL overrides the rule-cache TTL. Production default is 30s; tests
// use short values to verify the TTL fallback without sleeping.
func (s *GuardrailService) SetRulesTTL(d time.Duration) {
	s.mu.Lock()
	s.ttl = d
	s.mu.Unlock()
}

func (s *GuardrailService) getModelConfig(ctx context.Context, model string) *ModelGuardrailConfig {
	if model == "" {
		return nil
	}

	s.modelCfgMu.RLock()
	if entry, ok := s.modelCfgs[model]; ok && time.Since(entry.loaded) < s.ttl {
		s.modelCfgMu.RUnlock()
		return &entry.config
	}
	s.modelCfgMu.RUnlock()

	var row struct {
		ExtraConfig json.RawMessage `json:"extra_config"`
	}
	if err := s.db.WithContext(ctx).
		Table("provider_models").
		Select("extra_config").
		Where("model_name = ?", model).
		Limit(1).
		Find(&row).Error; err != nil {
		slog.Warn("guardrail: failed to load model config", "model", model, "error", err)
		return nil
	}
	var ec struct {
		Guardrails *ModelGuardrailConfig `json:"guardrails"`
	}
	if json.Unmarshal(row.ExtraConfig, &ec) != nil || ec.Guardrails == nil {
		return nil
	}

	s.modelCfgMu.Lock()
	s.modelCfgs[model] = modelCfgEntry{config: *ec.Guardrails, loaded: time.Now()}
	s.modelCfgMu.Unlock()

	return ec.Guardrails
}

func (s *GuardrailService) Check(ctx context.Context, req *CheckRequest) (*CheckResponse, error) {
	if err := s.ensureRules(ctx); err != nil {
		slog.Error("guardrail: failed to load rules", "error", err)
		if !s.failOpen.Load() {
			return nil, fmt.Errorf("guardrail: service unavailable: %w", err)
		}
		return &CheckResponse{Blocked: false}, nil
	}

	// Oversized content: either fail closed (reject) or inspect the prefix and
	// flag the result as partial coverage. Never a silent clean pass.
	truncated := false
	if utf8.RuneCountInString(req.Content) > maxCheckContent {
		if s.OversizeAction() == "reject" || s.StrictCoverage() || (s.StreamMode() == "strict" && req.Direction == DirectionResponse) {
			s.emitEvent(ctx, GuardrailEvent{
				EngineType: "guardrail_service",
				Direction:  string(req.Direction),
				Severity:   "high",
				Action:     "blocked",
				Reason:     fmt.Sprintf("content exceeds guardrail inspection limit (%d runes); oversize policy=reject", maxCheckContent),
				Model:      req.Model,
				APIKeyID:   req.APIKeyID,
				TeamID:     req.TeamID,
				OrgID:      req.OrgID,
				Timestamp:  time.Now(),
			})
			return &CheckResponse{
				Blocked:  true,
				Action:   "block",
				RuleName: "guardrail_oversize",
				Reason:   fmt.Sprintf("content exceeds guardrail inspection limit (%d runes) and oversize policy is reject", maxCheckContent),
			}, nil
		}
		runes := []rune(req.Content)
		req.Content = string(runes[:maxCheckContent])
		truncated = true
		s.emitEvent(ctx, GuardrailEvent{RuleName: "guardrail_partial_coverage", EngineType: "guardrail_service", Direction: string(req.Direction), Severity: "medium", Action: "log", Reason: "partial coverage: only the content prefix was inspected", Model: req.Model, APIKeyID: req.APIKeyID, TeamID: req.TeamID, OrgID: req.OrgID, Timestamp: time.Now()})
	}

	mc := s.getModelConfig(ctx, req.Model)
	if mc != nil && !mc.Enabled {
		return &CheckResponse{Blocked: false}, nil
	}

	s.mu.RLock()
	rules := make([]GuardrailRule, 0, len(s.rules))
	for _, r := range s.rules {
		dirMatch := r.Direction == string(req.Direction) || r.Direction == string(DirectionBoth)
		modelMatch := r.ModelFilter == nil || modelInFilter(req.Model, *r.ModelFilter)
		orgMatch := r.OrgID == nil || *r.OrgID == req.OrgID
		if dirMatch && modelMatch && orgMatch {
			rules = append(rules, r)
		}
	}
	s.mu.RUnlock()

	if mc != nil {
		var checks []string
		if req.Direction == DirectionRequest {
			checks = mc.RequestChecks
		} else {
			checks = mc.ResponseChecks
		}
		if len(checks) > 0 {
			allowed := make(map[string]bool, len(checks))
			for _, t := range checks {
				allowed[t] = true
			}
			filtered := make([]GuardrailRule, 0, len(rules))
			for _, r := range rules {
				if allowed[r.Type] {
					filtered = append(filtered, r)
				}
			}
			rules = filtered
		}
	}

	sort.SliceStable(rules, func(i, j int) bool {
		return severityOrder[rules[i].Severity] > severityOrder[rules[j].Severity]
	})

	logOnly := s.logOnly.Load()

	// Helper to read AgentType from CheckContext (updated by engines during loop)
	readAgentType := func() string {
		if v := ctx.Value(CtxKeyCheck); v != nil {
			if cc, ok := v.(*CheckContext); ok {
				return cc.AgentType
			}
		}
		return ""
	}

	resultReason := func(r *GuardrailResult) string {
		if r != nil {
			return r.Reason
		}
		return ""
	}

	var maskedContent string
	var maskedRuleName string
	var logRuleName string
	var maskedSpans []Span
	// matched collects every participating rule (evaluated and matched) for
	// decision attribution: the FINAL decision carries this list so no record
	// ever smears one rule's version across all rules.
	var matched []MatchedRule

	for _, rule := range rules {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		eng, err := s.getEngine(rule)
		if err != nil {
			slog.Error("guardrail: failed to create engine", "type", rule.Type, "error", err)
			recordCheckError(rule.Type, string(req.Direction))
			recordEngineError(rule.Type)
			s.emitEvent(ctx, GuardrailEvent{
				RuleID:      rule.ID,
				RuleName:    rule.Name,
				EngineType:  rule.Type,
				Direction:   string(req.Direction),
				Severity:    rule.Severity,
				Action:      "error",
				RuleVersion: rule.Version,
				Reason:      "engine creation failed",
				Model:       req.Model,
				APIKeyID:    req.APIKeyID,
				TeamID:      req.TeamID,
				OrgID:       req.OrgID,
				Timestamp:   time.Now(),
				AgentType:   readAgentType(),
			})
			if s.EngineErrorAction() == "block" && !s.failOpen.Load() {
				return nil, fmt.Errorf("guardrail: engine failure under block-on-error policy (%s): %w", rule.Type, err)
			}
			if severityOrder[rule.Severity] >= severityOrder["high"] && !s.failOpen.Load() {
				return nil, fmt.Errorf("guardrail: critical engine failure (%s): %w", rule.Type, err)
			}
			continue
		}

		result, err := eng.Check(ctx, req.Content, req.Direction, req.Model)
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			slog.Error("guardrail: engine check failed", "engine", eng.Name(), "error", err)
			recordCheckError(rule.Type, string(req.Direction))
			recordEngineError(rule.Type)
			s.emitEvent(ctx, GuardrailEvent{
				RuleID:      rule.ID,
				RuleName:    rule.Name,
				EngineType:  rule.Type,
				Direction:   string(req.Direction),
				Severity:    rule.Severity,
				Action:      "error",
				RuleVersion: rule.Version,
				Reason:      "engine check failed",
				Model:       req.Model,
				APIKeyID:    req.APIKeyID,
				TeamID:      req.TeamID,
				OrgID:       req.OrgID,
				Timestamp:   time.Now(),
				AgentType:   readAgentType(),
			})
			if s.EngineErrorAction() == "block" && !s.failOpen.Load() {
				return nil, fmt.Errorf("guardrail: engine failure under block-on-error policy (%s): %w", eng.Name(), err)
			}
			if severityOrder[rule.Severity] >= severityOrder["high"] && !s.failOpen.Load() {
				return nil, fmt.Errorf("guardrail: critical engine failure (%s): %w", eng.Name(), err)
			}
			continue
		}

		recordCheck(rule.Type, string(req.Direction), result != nil && result.Blocked)

		if result == nil || !result.Blocked {
			eventAction := "pass"
			if result != nil && result.Reason != "" {
				eventAction = rule.Action
				if eventAction == "" {
					eventAction = "log"
				}
			}
			s.emitEvent(ctx, GuardrailEvent{
				RuleID:      rule.ID,
				RuleName:    rule.Name,
				EngineType:  rule.Type,
				Direction:   string(req.Direction),
				Severity:    rule.Severity,
				Action:      eventAction,
				Reason:      resultReason(result),
				Model:       req.Model,
				APIKeyID:    req.APIKeyID,
				TeamID:      req.TeamID,
				OrgID:       req.OrgID,
				Timestamp:   time.Now(),
				AgentType:   readAgentType(),
				RuleVersion: rule.Version,
			})
			if eventAction == "log" {
				logRuleName = rule.Name
			}
			continue
		}

		action := rule.Action
		if action == "" {
			action = "block"
		}
		if logOnly && action == "block" {
			action = "log"
		}
		matched = append(matched, MatchedRule{RuleID: rule.ID, Version: rule.Version, Action: action})

		// This per-rule decision event carries the full participating list so
		// alert records attribute every contributing rule with its own version
		// — the last hit is never smeared across all rules.
		s.emitEvent(ctx, GuardrailEvent{
			RuleID:         rule.ID,
			RuleName:       rule.Name,
			EngineType:     rule.Type,
			Direction:      string(req.Direction),
			Severity:       rule.Severity,
			Action:         action,
			Reason:         result.Reason,
			Model:          req.Model,
			ContentPreview: result.ContentSnippet,
			APIKeyID:       req.APIKeyID,
			TeamID:         req.TeamID,
			OrgID:          req.OrgID,
			Timestamp:      time.Now(),
			AgentType:      readAgentType(),
			RuleVersion:    rule.Version,
			Matched:        append([]MatchedRule(nil), matched...),
		})

		if action == "log" {
			logRuleName = rule.Name
			continue
		}
		if action == "mask" {
			if len(result.MaskedSpans) > 0 {
				// Span-based masking: the engine located its matches; collect
				// for cross-rule composition. Spans are offsets into the
				// (possibly prefix-truncated) checked content, which is a
				// prefix of the caller's original content, so they remain
				// valid for protocol-aware callers.
				maskedSpans = append(maskedSpans, result.MaskedSpans...)
				maskedRuleName = rule.Name
			} else if result.MaskedContent != "" {
				maskedContent = result.MaskedContent
				maskedRuleName = rule.Name
			}
			continue
		}

		recordBlock(rule.Type, action)
		return &CheckResponse{
			Blocked:       true,
			RuleName:      rule.Name,
			Reason:        result.Reason,
			Action:        action,
			MaskedContent: maskedContent,
			Matched:       append([]MatchedRule(nil), matched...),
		}, nil
	}

	if len(maskedSpans) > 0 {
		composed := composeSpans(maskedSpans)
		return &CheckResponse{
			Blocked:       false,
			Action:        "mask",
			RuleName:      maskedRuleName,
			MaskedContent: applySpans(req.Content, composed),
			MaskedSpans:   composed,
			Truncated:     truncated,
			Matched:       append([]MatchedRule(nil), matched...),
		}, nil
	}
	if maskedContent != "" {
		return &CheckResponse{
			Blocked:       false,
			Action:        "mask",
			RuleName:      maskedRuleName,
			MaskedContent: maskedContent,
			Truncated:     truncated,
			Matched:       append([]MatchedRule(nil), matched...),
		}, nil
	}
	if logRuleName != "" {
		return &CheckResponse{Blocked: false, Action: "log", RuleName: logRuleName, Truncated: truncated, Matched: append([]MatchedRule(nil), matched...)}, nil
	}
	return &CheckResponse{Blocked: false, Truncated: truncated}, nil
}

// composeSpans merges spans from multiple mask rules into a non-overlapping
// set whose intervals are the UNION of all input matches: a shorter span
// overlapping a longer one must never leave the longer match's tail exposed.
// At equal start the longer span sorts first so its replacement wins; other
// overlaps keep the earlier (higher-severity — rules run severity-desc)
// replacement and extend its End to cover the union.
func composeSpans(spans []Span) []Span {
	if len(spans) == 0 {
		return nil
	}
	sorted := make([]Span, len(spans))
	copy(sorted, spans)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Start != sorted[j].Start {
			return sorted[i].Start < sorted[j].Start
		}
		return sorted[i].End > sorted[j].End
	})
	var out []Span
	for _, sp := range sorted {
		if sp.Start < 0 || sp.End <= sp.Start {
			continue
		}
		if n := len(out); n > 0 && sp.Start < out[n-1].End {
			if sp.End > out[n-1].End {
				out[n-1].End = sp.End
			}
			continue
		}
		out = append(out, sp)
	}
	return out
}

// applySpans returns content with each non-overlapping span replaced.
func applySpans(content string, spans []Span) string {
	if len(spans) == 0 {
		return content
	}
	var b strings.Builder
	last := 0
	for _, sp := range spans {
		if sp.Start < last || sp.End > len(content) || sp.Start >= sp.End {
			continue
		}
		b.WriteString(content[last:sp.Start])
		b.WriteString(sp.Replacement)
		last = sp.End
	}
	if last < len(content) {
		b.WriteString(content[last:])
	}
	return b.String()
}

func modelInFilter(model, filter string) bool {
	for _, m := range strings.Split(filter, ",") {
		if strings.TrimSpace(m) == model {
			return true
		}
	}
	return false
}

const maxCheckContent = 500000 // 500K runes

func (s *GuardrailService) RefreshSettings(ctx context.Context) {
	var settings []struct {
		Key   string `gorm:"column:key"`
		Value string `gorm:"column:value"`
	}
	if err := s.db.WithContext(ctx).Table("system_settings").
		Where("key IN ?", []string{
			"guardrails_enabled", "guardrails_log_only", "guardrails_fail_open",
			"guardrails_stream_mode", "guardrails_stream_strict_max_runes",
			"guardrails_stream_strict_max_bytes", "guardrails_stream_strict_max_events",
			"guardrails_request_budget_ms", "guardrails_response_budget_ms",
			"guardrails_oversize_action", "guardrails_engine_error_action",
			"guardrails_strict_coverage",
		}).
		Find(&settings).Error; err != nil {
		slog.Warn("guardrail: failed to refresh settings", "error", err)
		return
	}
	for _, st := range settings {
		switch st.Key {
		case "guardrails_enabled":
			s.enabled.Store(st.Value == "true")
		case "guardrails_log_only":
			s.logOnly.Store(st.Value == "true")
		case "guardrails_fail_open":
			s.failOpen.Store(st.Value == "true")
		case "guardrails_stream_mode":
			if st.Value == "strict" || st.Value == "window" {
				s.streamMode.Store(st.Value)
			} else {
				slog.Warn("guardrail: ignored invalid guardrails_stream_mode", "value", st.Value)
			}
		case "guardrails_stream_strict_max_runes":
			if n, err := strconv.Atoi(st.Value); err == nil && n > 0 {
				s.strictMaxRunes.Store(int64(n))
			}
		case "guardrails_stream_strict_max_bytes":
			if n, err := strconv.Atoi(st.Value); err == nil && n > 0 {
				s.strictMaxBytes.Store(int64(n))
			}
		case "guardrails_stream_strict_max_events":
			if n, err := strconv.Atoi(st.Value); err == nil && n > 0 {
				s.strictMaxEvents.Store(int64(n))
			}
		case "guardrails_request_budget_ms":
			if n, err := strconv.Atoi(st.Value); err == nil && n > 0 {
				s.requestBudgetMs.Store(int64(n))
			}
		case "guardrails_response_budget_ms":
			if n, err := strconv.Atoi(st.Value); err == nil && n > 0 {
				s.responseBudgetMs.Store(int64(n))
			}
		case "guardrails_oversize_action":
			if st.Value == "truncate" || st.Value == "reject" {
				s.oversizeAction.Store(st.Value)
			} else {
				slog.Warn("guardrail: ignored invalid guardrails_oversize_action", "value", st.Value)
			}
		case "guardrails_engine_error_action":
			if st.Value == "skip" || st.Value == "block" {
				s.engineErrorAction.Store(st.Value)
			} else {
				slog.Warn("guardrail: ignored invalid guardrails_engine_error_action", "value", st.Value)
			}
		case "guardrails_strict_coverage":
			s.strictCoverage.Store(st.Value == "true")
		}
	}
}
