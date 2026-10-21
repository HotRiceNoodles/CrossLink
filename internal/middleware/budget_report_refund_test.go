package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
)

// fakeBudgetSvc records settle calls (the optional SettleScope path) and
// satisfies BudgetServiceInterface with no-ops.
type fakeBudgetSvc struct {
	settles []settleCall
}

type settleCall struct {
	scope, targetID, period, pk, reqID string
	cost                               float64
}

func (f *fakeBudgetSvc) SettleScope(ctx context.Context, scope, targetID, period, periodKey, reqID string, cost float64) {
	f.settles = append(f.settles, settleCall{scope, targetID, period, periodKey, reqID, cost})
}

func (f *fakeBudgetSvc) CheckBudget(ctx context.Context, scope, targetID, period string, budgetLimit float64) (float64, float64, bool) {
	return 0, budgetLimit, false
}
func (f *fakeBudgetSvc) ReportUsage(ctx context.Context, scope, targetID, period string, cost float64) {}
func (f *fakeBudgetSvc) AdjustBudget(ctx context.Context, scope, targetID, period string, delta float64) {
}
func (f *fakeBudgetSvc) AdjustBudgetKeyed(ctx context.Context, scope, targetID, period, periodKey string, delta float64) {
}
func (f *fakeBudgetSvc) CheckCallLimit(ctx context.Context, keyID, period string, maxCalls int) (int, bool) {
	return 0, false
}
func (f *fakeBudgetSvc) ReserveCall(ctx context.Context, keyID, period string, maxCalls int) (int, bool, string) {
	return 0, false, ""
}
func (f *fakeBudgetSvc) RefundCall(ctx context.Context, keyID, periodKey string) {}
func (f *fakeBudgetSvc) ReportCallUsage(ctx context.Context, keyID, period string) {
}

func newReportTestCtx(t *testing.T, apiKeyPresent bool) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if apiKeyPresent {
		c.Set("api_key", &model.APIKey{MaxBudget: 10, BudgetPeriod: "monthly"})
	}
	return c, w
}

// r33c residual root: a request that dies BEFORE any token accounting (no
// input_tokens/output_tokens/cost on the context) but holds F1' reservations
// MUST still settle cost=0 (refund the marks) — the old early-return skipped
// settlement entirely, leaving live marks whose settles never ran (the
// "never-settled cohort": settled counter vs DB gap + marks pending reaper).
func TestReportBudgetUsage_TokenlessRequestWithReservationsStillSettles(t *testing.T) {
	svc := &fakeBudgetSvc{}
	c, _ := newReportTestCtx(t, true)
	c.Set("request_id", "req-refund-1")
	// Key scope fires via apiKey.MaxBudget; team/org scopes stay out (nil
	// caches) — the assertion is about the EARLY-EXIT, not scope coverage.
	c.Keys["api_key"] = &model.APIKey{MaxBudget: 10, BudgetPeriod: "monthly"}
	c.Set("budget_reservations", &service.BudgetReservations{
		Reserved:   map[string]float64{"key": 0.5},
		PeriodKeys: map[string]string{"key": "2026-10"},
	})

	mw := ReportBudgetUsage(svc, nil, nil, nil)
	mw(c) // runs c.Next (no handler) and the deferred settlement

	if len(svc.settles) == 0 {
		t.Fatal("no SettleScope calls: token-less request with reservations was skipped (marks leak until reaper)")
	}
	for _, s := range svc.settles {
		if s.cost != 0 {
			t.Fatalf("settle cost = %v, want 0 (refund) for scope %s", s.cost, s.scope)
		}
		if s.reqID != "req-refund-1" {
			t.Fatalf("settle reqID = %q, want the request id", s.reqID)
		}
	}
}

// No reservations and no token usage: the early-exit MUST still fire (no
// ledger divergence, no noise settles).
func TestReportBudgetUsage_TokenlessNoReservationsSkips(t *testing.T) {
	svc := &fakeBudgetSvc{}
	c, _ := newReportTestCtx(t, true)
	mw := ReportBudgetUsage(svc, nil, nil, nil)
	mw(c)
	if len(svc.settles) != 0 {
		t.Fatalf("unexpected settles: %+v", svc.settles)
	}
}

// FINDING-B regression at the middleware layer: a scope whose reserve failed
// (no PeriodKeys entry — the outage-window shape) must settle into the
// CURRENT period key, not an empty one.
func TestReportBudgetUsage_ReserveFailedScopeSettlesCurrentPeriod(t *testing.T) {
	svc := &fakeBudgetSvc{}
	c, _ := newReportTestCtx(t, true)
	c.Keys["api_key"] = &model.APIKey{MaxBudget: 10, BudgetPeriod: "monthly"}
	c.Set("request_id", "req-fb-mw-1")
	c.Set("input_tokens", 10)
	c.Set("input_price", 0.001)
	// Reserve failed: reservations struct exists but PeriodKeys empty.
	c.Set("budget_reservations", &service.BudgetReservations{
		Reserved: map[string]float64{"key": 0.5},
	})
	mw := ReportBudgetUsage(svc, nil, nil, nil)
	mw(c)
	if len(svc.settles) == 0 {
		t.Fatal("no settle for a scope with failed reserve")
	}
	cur := service.PeriodKey("monthly")
	for _, s := range svc.settles {
		if s.pk == "" {
			t.Fatalf("settle carried empty period key for scope %s — orphan-key bug", s.scope)
		}
		if s.pk != cur {
			t.Fatalf("settle pk=%q, want current period %q", s.pk, cur)
		}
	}
}
