package middleware

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// r61 finding lock: a team/org metadata lookup failure must be COUNTED and
// (sampled) logged — the scope drop itself stays fail-open (availability
// posture), but never silent again.
func TestBudgetScopeDegrade_Counted(t *testing.T) {
	before := testutil.ToFloat64(budgetScopeDegradeTotal.WithLabelValues("team", "rejected_cold"))
	noteBudgetScopeDegrade("team", 990624)
	noteBudgetScopeDegrade("team", 990624)
	if got := testutil.ToFloat64(budgetScopeDegradeTotal.WithLabelValues("team", "rejected_cold")) - before; got != 2 {
		t.Fatalf("team degrade delta = %v, want 2", got)
	}
	beforeOrg := testutil.ToFloat64(budgetScopeDegradeTotal.WithLabelValues("org", "rejected_cold"))
	noteBudgetScopeDegrade("org", 990623)
	if got := testutil.ToFloat64(budgetScopeDegradeTotal.WithLabelValues("org", "rejected_cold")) - beforeOrg; got != 1 {
		t.Fatalf("org degrade delta = %v, want 1", got)
	}
}
