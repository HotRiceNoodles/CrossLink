package service

import (
	"context"
	"testing"
	"time"

	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
)

// mutableLoader is a ruleLoader whose rule set can be swapped at runtime to simulate
// the classifier's background refresh picking up a changed rule table.
type mutableLoader struct{ rules []ruleView }

func (m *mutableLoader) ListEnabled(context.Context) ([]ruleView, error) { return m.rules, nil }

// TestIntegration_PersistentQuotaTriggersFallbackAndCircuit wires the full stack
// (ErrorClassifier + HealthTracker + FallbackEngine): a single insufficient_quota
// error on provider A is classified persistent, opens the account circuit on A, and
// triggers an immediate fallback to provider B. Subsequent routing sees A as unhealthy.
func TestIntegration_PersistentQuotaTriggersFallbackAndCircuit(t *testing.T) {
	// Transient threshold/cooldown are irrelevant here — only the persistent path fires.
	health := provider.NewHealthTrackerWithConfig(3, time.Minute)
	classifier := NewErrorClassifier(&mutableLoader{rules: []ruleView{
		{MatchField: "code", Pattern: "insufficient_quota", ProviderType: ptrS("openai_compatible"), Classification: "quota", Scope: "account"},
	}}, 0)

	engine := NewFallbackEngine(health, router.FallbackConfig{})
	engine.SetClassifier(classifier)

	routes := []*router.RouteResult{
		makeRouteTyped("A", "openai_compatible"),
		makeRouteTyped("B", "openai_compatible"),
	}
	quotaErr := &provider.ProviderError{StatusCode: 429, Code: "insufficient_quota", ErrorType: provider.ErrorRateLimit, Message: "insufficient_quota"}

	result := engine.ExecuteNonStream(context.Background(), routes, func(_ context.Context, route *router.RouteResult) (any, error) {
		if route.Provider.Name() == "A" {
			return nil, quotaErr
		}
		return "ok-from-B", nil
	})

	// One persistent failure on A → fallback to B on the first strike.
	if result.FinalError != nil {
		t.Fatalf("expected success via fallback, got %v", result.FinalError)
	}
	if got := result.Route.Provider.Name(); got != "B" {
		t.Fatalf("expected serving route B, got %s", got)
	}
	if result.FallbackCount != 1 {
		t.Fatalf("expected FallbackCount=1, got %d", result.FallbackCount)
	}
	if !result.Attempts[0].Persistent {
		t.Fatal("A attempt must be recorded as persistent")
	}

	// The account-scoped persistent circuit on A is now open — A is unhealthy for
	// any model, so the next request would skip A entirely.
	if health.IsHealthyModel("A", "any-model") {
		t.Fatal("A account circuit must be open after a persistent failure")
	}
	// B remains healthy (it succeeded).
	if !health.IsHealthyModel("B", "test-model") {
		t.Fatal("B must remain healthy after success")
	}
}

// TestIntegration_ClassifierReloadDowngradesPersistent verifies that once the rule
// matching insufficient_quota is removed and the classifier reloads, the same upstream
// error degrades to transient — i.e. operators can stop over-classifying without a
// restart by editing the rule table.
func TestIntegration_ClassifierReloadDowngradesPersistent(t *testing.T) {
	loader := &mutableLoader{rules: []ruleView{
		{MatchField: "code", Pattern: "insufficient_quota", ProviderType: ptrS("openai_compatible"), Classification: "quota", Scope: "account"},
	}}
	classifier := NewErrorClassifier(loader, 0)
	pe := &provider.ProviderError{StatusCode: 429, Code: "insufficient_quota", ErrorType: provider.ErrorRateLimit}

	if !classifier.Classify("openai_compatible", pe).Persistent {
		t.Fatal("insufficient_quota must be persistent while the rule exists")
	}

	// Operator deletes the rule; the refresh loop reloads an empty set.
	loader.rules = nil
	if err := classifier.load(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if classifier.Classify("openai_compatible", pe).Persistent {
		t.Fatal("after the rule is removed the same error must be transient")
	}
}
