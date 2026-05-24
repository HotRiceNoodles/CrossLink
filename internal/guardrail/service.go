package guardrail

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
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
	db      *gorm.DB
	deps    EngineDeps
	enabled atomic.Bool
	logOnly atomic.Bool
	failOpen atomic.Bool

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

func NewGuardrailService(db *gorm.DB, rdb redis.Cmdable) *GuardrailService {
	return &GuardrailService{
		db:        db,
		deps:      EngineDeps{Redis: rdb, DB: db},
		ttl:       30 * time.Second,
		engines:   make(map[int64]GuardrailEngine),
		modelCfgs: make(map[string]modelCfgEntry),
		failOpen:  atomic.Bool{}, // default false: DB failure = error
	}
}

func (s *GuardrailService) SetFailOpen(v bool)        { s.failOpen.Store(v) }
func (s *GuardrailService) IsFailOpen() bool             { return s.failOpen.Load() }

func (s *GuardrailService) SetIsDebugEnabled(fn func() bool)      { s.deps.IsDebugEnabled = fn }
func (s *GuardrailService) SetCheckLicenseTier(fn func() string)  { s.deps.CheckLicenseTier = fn }
func (s *GuardrailService) SetRedisClient(c *redis.Client)        { s.deps.RedisClient = c }

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
			defer func() {
				if r := recover(); r != nil {
					slog.Error("guardrail: handler panic", "error", r)
				}
			}()
			timeoutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			h(timeoutCtx, event)
		}(h)
	}
}
func (s *GuardrailService) IsEnabled() bool  { return s.enabled.Load() }
func (s *GuardrailService) SetEnabled(v bool) { s.enabled.Store(v) }

func (s *GuardrailService) IsLogOnly() bool  { return s.logOnly.Load() }
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

	// Truncate oversized content to prevent CPU pressure on regex engines
	if utf8.RuneCountInString(req.Content) > maxCheckContent {
		runes := []rune(req.Content)
		req.Content = string(runes[:maxCheckContent])
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
		if dirMatch && modelMatch {
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

	for _, rule := range rules {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		eng, err := s.getEngine(rule)
		if err != nil {
			slog.Error("guardrail: failed to create engine", "type", rule.Type, "error", err)
			s.emitEvent(ctx, GuardrailEvent{
				RuleID:     rule.ID,
				RuleName:   rule.Name,
				EngineType: rule.Type,
				Direction:  string(req.Direction),
				Severity:   rule.Severity,
				Action:     "error",
				Reason:     "engine creation failed",
				Model:      req.Model,
				APIKeyID:   req.APIKeyID,
				TeamID:     req.TeamID,
				Timestamp:  time.Now(),
				AgentType:  readAgentType(),
			})
			if severityOrder[rule.Severity] >= severityOrder["high"] && !s.failOpen.Load() {
				return nil, fmt.Errorf("guardrail: critical engine failure (%s): %w", rule.Type, err)
			}
			continue
		}

		result, err := eng.Check(ctx, req.Content, req.Direction, req.Model)
		if err != nil {
			slog.Error("guardrail: engine check failed", "engine", eng.Name(), "error", err)
			s.emitEvent(ctx, GuardrailEvent{
				RuleID:     rule.ID,
				RuleName:   rule.Name,
				EngineType: rule.Type,
				Direction:  string(req.Direction),
				Severity:   rule.Severity,
				Action:     "error",
				Reason:     "engine check failed",
				Model:      req.Model,
				APIKeyID:   req.APIKeyID,
				TeamID:     req.TeamID,
				Timestamp:  time.Now(),
				AgentType:  readAgentType(),
			})
			if severityOrder[rule.Severity] >= severityOrder["high"] && !s.failOpen.Load() {
				return nil, fmt.Errorf("guardrail: critical engine failure (%s): %w", eng.Name(), err)
			}
			continue
		}

		if result == nil || !result.Blocked {
			eventAction := "pass"
			if result != nil && result.Reason != "" {
				eventAction = rule.Action
				if eventAction == "" {
					eventAction = "log"
				}
			}
			s.emitEvent(ctx, GuardrailEvent{
				RuleID:     rule.ID,
				RuleName:   rule.Name,
				EngineType: rule.Type,
				Direction:  string(req.Direction),
				Severity:   rule.Severity,
				Action:     eventAction,
				Reason:     resultReason(result),
				Model:      req.Model,
				APIKeyID:   req.APIKeyID,
				TeamID:     req.TeamID,
				Timestamp:  time.Now(),
				AgentType:  readAgentType(),
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
			Timestamp:      time.Now(),
			AgentType:      readAgentType(),
		})

		if action == "log" {
			logRuleName = rule.Name
			continue
		}
		if action == "mask" && result.MaskedContent != "" {
			maskedContent = result.MaskedContent
			maskedRuleName = rule.Name
			continue
		}

		return &CheckResponse{
			Blocked:       true,
			RuleName:      rule.Name,
			Reason:        result.Reason,
			Action:        action,
			MaskedContent: maskedContent,
		}, nil
	}

	if maskedContent != "" {
		return &CheckResponse{
			Blocked:       false,
			Action:        "mask",
			RuleName:      maskedRuleName,
			MaskedContent: maskedContent,
		}, nil
	}
	if logRuleName != "" {
		return &CheckResponse{Blocked: false, Action: "log", RuleName: logRuleName}, nil
	}
	return &CheckResponse{Blocked: false}, nil
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
		Where("key IN ?", []string{"guardrails_enabled", "guardrails_log_only", "guardrails_fail_open"}).
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
		}
	}
}