package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crosslink/internal/provider"
)

type stubLoader []ruleView

func (s stubLoader) ListEnabled(context.Context) ([]ruleView, error) { return s, nil }

type flakyLoader struct {
	rules []ruleView
	fail  bool
}

func (f *flakyLoader) ListEnabled(context.Context) ([]ruleView, error) {
	if f.fail {
		return nil, errors.New("db down")
	}
	return f.rules, nil
}

func ptrS(s string) *string { return &s }

func TestClassify_PersistentByCode_ScopeFromRule(t *testing.T) {
	c := NewErrorClassifier(stubLoader{
		{MatchField: "code", Pattern: "insufficient_quota", ProviderType: ptrS("openai_compatible"), Classification: "quota", Scope: "account"},
	}, time.Hour)
	got := c.Classify("openai_compatible", &provider.ProviderError{StatusCode: 429, Code: "insufficient_quota"})
	if !got.Persistent || got.ErrorType != provider.ErrorQuota || got.Scope != "account" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestClassify_ModelDeprecated_ScopeModel(t *testing.T) {
	c := NewErrorClassifier(stubLoader{
		{MatchField: "type", Pattern: "model_deprecated", ProviderType: ptrS("openai_compatible"), Classification: "quota", Scope: "model"},
	}, time.Hour)
	got := c.Classify("openai_compatible", &provider.ProviderError{StatusCode: 404, Type: "model_deprecated"})
	if !got.Persistent || got.Scope != "model" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestClassify_NonProviderError_DefaultsTransient(t *testing.T) {
	c := NewErrorClassifier(stubLoader{
		{MatchField: "code", Pattern: "anything", Classification: "quota", Scope: "account"},
	}, time.Hour)
	got := c.Classify("openai_compatible", errors.New("some network error"))
	if got.Persistent {
		t.Fatal("non-*ProviderError must be transient")
	}
}

func TestClassify_ProviderTypeFilter(t *testing.T) {
	c := NewErrorClassifier(stubLoader{
		{MatchField: "code", Pattern: "x", ProviderType: ptrS("anthropic"), Classification: "quota", Scope: "account"},
	}, time.Hour)
	got := c.Classify("openai_compatible", &provider.ProviderError{Code: "x"})
	if got.Persistent {
		t.Fatal("anthropic-scoped rule must not match an openai_compatible error")
	}
}

func TestClassify_ExactMatchNotSubstring(t *testing.T) {
	c := NewErrorClassifier(stubLoader{
		{MatchField: "code", Pattern: "quota", Classification: "quota", Scope: "account"},
	}, time.Hour)
	// code is matched exactly; "disk quota exceeded" must NOT match pattern "quota".
	got := c.Classify("openai_compatible", &provider.ProviderError{Code: "disk quota exceeded"})
	if got.Persistent {
		t.Fatal("code match must be exact, not substring")
	}
}

func TestClassify_TieBreak_SpecificBeforeGlobal(t *testing.T) {
	// Same priority: global status=402 (account) vs specific code=x (model).
	// Both match a 402 from openai with code=x. Specific must win → scope=model.
	c := NewErrorClassifier(stubLoader{
		{ID: 1, MatchField: "status", Pattern: "402", Classification: "quota", Scope: "account", Priority: 100},
		{ID: 2, MatchField: "code", Pattern: "x", ProviderType: ptrS("openai_compatible"), Classification: "quota", Scope: "model", Priority: 100},
	}, time.Hour)
	got := c.Classify("openai_compatible", &provider.ProviderError{StatusCode: 402, Code: "x"})
	if !got.Persistent || got.Scope != "model" {
		t.Fatalf("specific rule must win over global, got %+v", got)
	}
}

func TestClassify_TieBreak_AccountBeforeModel(t *testing.T) {
	// Same priority, both global: account scope should win over model scope.
	c := NewErrorClassifier(stubLoader{
		{ID: 1, MatchField: "status", Pattern: "402", Classification: "quota", Scope: "model", Priority: 100},
		{ID: 2, MatchField: "status", Pattern: "402", Classification: "quota", Scope: "account", Priority: 100},
	}, time.Hour)
	got := c.Classify("openai_compatible", &provider.ProviderError{StatusCode: 402})
	if !got.Persistent || got.Scope != "account" {
		t.Fatalf("account scope must win over model, got %+v", got)
	}
}

func TestClassify_LoadFailure_KeepsLastKnownGood(t *testing.T) {
	l := &flakyLoader{rules: []ruleView{{MatchField: "code", Pattern: "insufficient_quota", Classification: "quota", Scope: "account"}}}
	c := NewErrorClassifier(l, time.Hour) // initial load succeeds
	l.fail = true
	if err := c.load(context.Background()); err == nil {
		t.Fatal("expected load to fail")
	}
	got := c.Classify("openai_compatible", &provider.ProviderError{Code: "insufficient_quota"})
	if !got.Persistent {
		t.Fatal("last-known-good rules must be retained after a failed reload")
	}
}
