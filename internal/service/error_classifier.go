package service

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
)

// ClassifiedError is the outcome of classifying an upstream error.
type ClassifiedError struct {
	ErrorType  provider.ErrorType
	Persistent bool
	Scope      string // "account"|"model" — from the matched rule; empty when not persistent
}

// ruleView is the in-memory representation of an enabled error_classification_rule.
type ruleView struct {
	ID             int64
	MatchField     string // status|code|type|message
	Pattern        string
	ProviderType   *string // nil = global
	Scope          string  // account|model
	Classification string  // quota (persistent); transient reserved
	Priority       int
}

// matches reports whether the rule applies to the given provider error.
// status/code/type are matched exactly (case-insensitive); message is substring.
func (r ruleView) matches(adapterType string, pe *provider.ProviderError) bool {
	if r.ProviderType != nil && *r.ProviderType != adapterType {
		return false
	}
	switch r.MatchField {
	case "code":
		return pe.Code != "" && strings.EqualFold(pe.Code, r.Pattern)
	case "type":
		return pe.Type != "" && strings.EqualFold(pe.Type, r.Pattern)
	case "status":
		return strconv.Itoa(pe.StatusCode) == r.Pattern
	case "message":
		return r.Pattern != "" && strings.Contains(strings.ToLower(pe.Message), strings.ToLower(r.Pattern))
	default:
		return false
	}
}

// ruleLoader provides enabled rules already mapped to ruleView.
type ruleLoader interface {
	ListEnabled(context.Context) ([]ruleView, error)
}

// ErrorRuleLister is implemented by *repository.ErrorRuleRepo.
type ErrorRuleLister interface {
	ListEnabled(context.Context) ([]model.ErrorClassificationRule, error)
}

type ruleLoaderFunc func(context.Context) ([]ruleView, error)

func (f ruleLoaderFunc) ListEnabled(ctx context.Context) ([]ruleView, error) { return f(ctx) }

// AdaptErrorRuleLoader wraps a model-returning repo into the classifier's ruleLoader.
func AdaptErrorRuleLoader(repo ErrorRuleLister) ruleLoader {
	return ruleLoaderFunc(func(ctx context.Context) ([]ruleView, error) {
		ms, err := repo.ListEnabled(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]ruleView, len(ms))
		for i, m := range ms {
			out[i] = ruleView{
				ID:             m.ID,
				MatchField:     m.MatchField,
				Pattern:        m.Pattern,
				ProviderType:   m.ProviderType,
				Scope:          m.Scope,
				Classification: m.Classification,
				Priority:       m.Priority,
			}
		}
		return out, nil
	})
}

// ErrorClassifier classifies upstream errors as persistent or transient using a
// TTL-cached rule table. On load failure the last-known-good set is retained
// (never cleared — clearing would make persistent errors degrade to transient and
// cause dead upstreams to be hammered).
type ErrorClassifier struct {
	loader   ruleLoader
	rules    atomic.Pointer[[]ruleView]
	interval time.Duration
}

func NewErrorClassifier(loader ruleLoader, interval time.Duration) *ErrorClassifier {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	c := &ErrorClassifier{loader: loader, interval: interval}
	if err := c.load(context.Background()); err != nil {
		slog.Error("error classifier initial load failed; rules empty until next refresh", "error", err)
	}
	return c
}

// Classify returns the classification for err. Non-*ProviderError errors (network,
// timeout, cancellation) are transient by default and never query the rule table.
// Rules are evaluated in tie-break order (priority ASC, specific before global,
// account before model, id ASC); the first quota match wins.
func (c *ErrorClassifier) Classify(adapterType string, err error) ClassifiedError {
	pe, ok := err.(*provider.ProviderError)
	if !ok {
		return ClassifiedError{ErrorType: provider.ClassifyError(err)}
	}
	if p := c.rules.Load(); p != nil {
		for _, r := range *p {
			if r.Classification == "quota" && r.matches(adapterType, pe) {
				return ClassifiedError{ErrorType: provider.ErrorQuota, Persistent: true, Scope: r.Scope}
			}
		}
	}
	return ClassifiedError{ErrorType: pe.ErrorType}
}

// load fetches rules, sorts them into tie-break order, and atomically swaps them in.
func (c *ErrorClassifier) load(ctx context.Context) error {
	rs, err := c.loader.ListEnabled(ctx)
	if err != nil {
		return err
	}
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		// specific (non-nil provider_type) before global (nil)
		ai := a.ProviderType != nil
		bj := b.ProviderType != nil
		if ai != bj {
			return ai // specific first
		}
		// account before model
		if a.Scope != b.Scope {
			return a.Scope == "account"
		}
		return a.ID < b.ID
	})
	c.rules.Store(&rs)
	return nil
}

// RunRefreshLoop periodically reloads rules until ctx is cancelled.
func (c *ErrorClassifier) RunRefreshLoop(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.load(ctx); err != nil {
				slog.Error("error classifier refresh failed; keeping last-known-good rules", "error", err)
			}
		}
	}
}
