package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/crosslink/internal/model"
)

// mockBudgetSvc implements service.BudgetServiceInterface for testing usage_query
// without Redis. Only the two read methods are exercised; report methods are no-ops.
type mockBudgetSvc struct {
	budgetSpent    float64
	budgetLimit    float64
	budgetExceeded bool
	callsUsed      int
	callsExceeded  bool
}

func (m *mockBudgetSvc) CheckBudget(_ context.Context, _, _, _ string, _ float64) (float64, float64, bool) {
	return m.budgetSpent, m.budgetLimit, m.budgetExceeded
}
func (m *mockBudgetSvc) ReportUsage(_ context.Context, _, _, _ string, _ float64)        {}
func (m *mockBudgetSvc) CheckCallLimit(_ context.Context, _, _ string, _ int) (int, bool) { return m.callsUsed, m.callsExceeded }
func (m *mockBudgetSvc) ReportCallUsage(_ context.Context, _, _ string)                   {}

func newUsageCtx(key *model.APIKey) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	if key != nil {
		c.Set("api_key", key)
	}
	return c, w
}

func parseUsageBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	return body
}

func TestUsageQuery_PostAuthNilKey(t *testing.T) {
	h := NewUsageQueryHandler(&mockBudgetSvc{})
	c, w := newUsageCtx(nil) // config-authkey fallback: no *model.APIKey in context
	h.GetUsage(c)

	assert.Equal(t, http.StatusForbidden, w.Code)
	body := parseUsageBody(t, w)
	errObj, ok := body["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "permission_error", errObj["type"])
}

func TestUsageQuery_NilBudgetSvc(t *testing.T) {
	h := NewUsageQueryHandler(nil)
	c, w := newUsageCtx(&model.APIKey{ID: 1})
	h.GetUsage(c)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	body := parseUsageBody(t, w)
	errObj, _ := body["error"].(map[string]any)
	assert.Equal(t, "service_unavailable", errObj["type"])
}

func TestUsageQuery_BudgetActive(t *testing.T) {
	exp := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	h := NewUsageQueryHandler(&mockBudgetSvc{
		budgetSpent: 37.5231, budgetLimit: 100, budgetExceeded: false,
		callsUsed: 412, callsExceeded: false,
	})
	c, w := newUsageCtx(&model.APIKey{
		ID: 123, Status: 1, ExpiresAt: &exp,
		MaxBudget: 100, BudgetPeriod: "monthly",
		MaxCalls: 1000, CallPeriod: "daily",
		TPMLimit: 60000, RPMLimit: 60,
	})
	h.GetUsage(c)

	assert.Equal(t, http.StatusOK, w.Code)
	body := parseUsageBody(t, w)

	ak := body["api_key"].(map[string]any)
	assert.Equal(t, float64(123), ak["id"])
	assert.Equal(t, float64(1), ak["status"])
	assert.Equal(t, "2026-12-31T23:59:59Z", ak["expires_at"])
	// name / key_prefix must NOT be exposed
	_, hasName := ak["name"]
	_, hasPrefix := ak["key_prefix"]
	assert.False(t, hasName, "name must not be exposed")
	assert.False(t, hasPrefix, "key_prefix must not be exposed")

	budget := body["budget"].(map[string]any)
	assert.Equal(t, "monthly", budget["period"])
	assert.Equal(t, 100.0, budget["limit"])
	assert.InDelta(t, 37.5231, budget["spent"], 1e-9)
	assert.InDelta(t, 62.4769, budget["remaining"], 1e-9)
	assert.Equal(t, false, budget["exceeded"])

	calls := body["calls"].(map[string]any)
	assert.Equal(t, "daily", calls["period"])
	assert.Equal(t, 1000.0, calls["limit"])
	assert.Equal(t, 412.0, calls["used"])
	assert.Equal(t, 588.0, calls["remaining"])
	assert.Equal(t, false, calls["exceeded"])

	limits := body["limits"].(map[string]any)
	assert.Equal(t, 60000.0, limits["tpm_limit"])
	assert.Equal(t, 60.0, limits["rpm_limit"])
}

func TestUsageQuery_BudgetAtExactlyLimit(t *testing.T) {
	// >= semantics: spent == limit => exceeded=true, remaining=0.
	h := NewUsageQueryHandler(&mockBudgetSvc{
		budgetSpent: 100, budgetLimit: 100, budgetExceeded: true,
	})
	c, w := newUsageCtx(&model.APIKey{ID: 1, MaxBudget: 100, BudgetPeriod: "monthly"})
	h.GetUsage(c)

	assert.Equal(t, http.StatusOK, w.Code)
	budget := parseUsageBody(t, w)["budget"].(map[string]any)
	assert.Equal(t, true, budget["exceeded"])
	assert.Equal(t, 0.0, budget["remaining"])
}

func TestUsageQuery_UnlimitedBudget(t *testing.T) {
	// MaxBudget=0 => CheckBudget returns (0,0,false); limit is the unlimited sentinel.
	h := NewUsageQueryHandler(&mockBudgetSvc{}) // zero-value: spent=0, limit=0, exceeded=false
	c, w := newUsageCtx(&model.APIKey{ID: 1, MaxBudget: 0, BudgetPeriod: "monthly"})
	h.GetUsage(c)

	assert.Equal(t, http.StatusOK, w.Code)
	budget := parseUsageBody(t, w)["budget"].(map[string]any)
	assert.Equal(t, 0.0, budget["limit"])
	assert.Equal(t, 0.0, budget["spent"])
	assert.Equal(t, 0.0, budget["remaining"])
	assert.Equal(t, false, budget["exceeded"])
}

func TestUsageQuery_UnlimitedCalls(t *testing.T) {
	h := NewUsageQueryHandler(&mockBudgetSvc{})
	c, w := newUsageCtx(&model.APIKey{ID: 1, MaxCalls: 0, CallPeriod: "daily"})
	h.GetUsage(c)

	assert.Equal(t, http.StatusOK, w.Code)
	calls := parseUsageBody(t, w)["calls"].(map[string]any)
	assert.Equal(t, 0.0, calls["limit"])
	assert.Equal(t, 0.0, calls["used"])
	assert.Equal(t, 0.0, calls["remaining"])
	assert.Equal(t, false, calls["exceeded"])
}

func TestUsageQuery_NilExpiresAt(t *testing.T) {
	h := NewUsageQueryHandler(&mockBudgetSvc{})
	c, w := newUsageCtx(&model.APIKey{ID: 1, ExpiresAt: nil})
	h.GetUsage(c)

	assert.Equal(t, http.StatusOK, w.Code)
	ak := parseUsageBody(t, w)["api_key"].(map[string]any)
	assert.Nil(t, ak["expires_at"])
}
