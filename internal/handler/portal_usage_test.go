package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/crosslink/internal/model"
)

// Portal contract tests: the response must expose a CLEAN shape (booleans for
// active/unlimited) so the portal frontend never needs to know the raw
// status int, the limit==0 sentinel, or snake_case internals. These are the
// decoupling boundary.

func TestPortalUsage_PostAuthNilKey(t *testing.T) {
	h := NewPortalUsageHandler(&mockBudgetSvc{})
	c, w := newUsageCtx(nil)
	h.GetUsage(c)

	assert.Equal(t, http.StatusForbidden, w.Code)
	errObj := parseUsageBody(t, w)["error"].(map[string]any)
	assert.Equal(t, "permission_error", errObj["type"])
}

func TestPortalUsage_NilBudgetSvc(t *testing.T) {
	h := NewPortalUsageHandler(nil)
	c, w := newUsageCtx(&model.APIKey{ID: 1})
	h.GetUsage(c)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestPortalUsage_ActiveKeyCleanContract(t *testing.T) {
	exp := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	h := NewPortalUsageHandler(&mockBudgetSvc{
		budgetSpent: 37.5, budgetLimit: 100,
		callsUsed: 412,
	})
	c, w := newUsageCtx(&model.APIKey{
		ID: 123, Status: 1, ExpiresAt: &exp,
		MaxBudget: 100, BudgetPeriod: "monthly",
		MaxCalls: 1000, CallPeriod: "daily",
		TPMLimit: 60000, RPMLimit: 60,
	})
	h.GetUsage(c)

	require.Equal(t, http.StatusOK, w.Code)
	body := parseUsageBody(t, w)

	// key: clean booleans, no raw status int, no id/name/prefix leakage
	key := body["key"].(map[string]any)
	assert.Equal(t, true, key["active"])
	assert.Equal(t, "2026-12-31T23:59:59Z", key["expires_at"])
	_, hasStatus := key["status"]
	assert.False(t, hasStatus, "raw status must not leak")

	budget := body["budget"].(map[string]any)
	assert.Equal(t, "monthly", budget["period"])
	assert.Equal(t, 100.0, budget["limit"])
	assert.InDelta(t, 37.5, budget["current"], 1e-9)
	assert.InDelta(t, 62.5, budget["remaining"], 1e-9)
	assert.Equal(t, false, budget["unlimited"])
	assert.Equal(t, false, budget["exceeded"])

	calls := body["calls"].(map[string]any)
	assert.Equal(t, 1000.0, calls["limit"])
	assert.Equal(t, 412.0, calls["current"])
	assert.Equal(t, 588.0, calls["remaining"])
	assert.Equal(t, false, calls["unlimited"])

	limits := body["limits"].(map[string]any)
	assert.Equal(t, 60000.0, limits["tpm"])
	assert.Equal(t, 60.0, limits["rpm"])
	assert.Equal(t, false, limits["tpm_unlimited"])
	assert.Equal(t, false, limits["rpm_unlimited"])
}

func TestPortalUsage_DisabledKeyActiveFalse(t *testing.T) {
	h := NewPortalUsageHandler(&mockBudgetSvc{})
	c, w := newUsageCtx(&model.APIKey{ID: 1, Status: 2}) // status != 1 → not active
	h.GetUsage(c)
	require.Equal(t, http.StatusOK, w.Code)
	key := parseUsageBody(t, w)["key"].(map[string]any)
	assert.Equal(t, false, key["active"])
}

func TestPortalUsage_UnlimitedBudgetAndCalls(t *testing.T) {
	h := NewPortalUsageHandler(&mockBudgetSvc{}) // zero → spent=0, limit=0
	c, w := newUsageCtx(&model.APIKey{
		ID: 1, MaxBudget: 0, BudgetPeriod: "monthly",
		MaxCalls: 0, CallPeriod: "daily",
	})
	h.GetUsage(c)
	require.Equal(t, http.StatusOK, w.Code)
	body := parseUsageBody(t, w)
	assert.Equal(t, true, body["budget"].(map[string]any)["unlimited"])
	assert.Equal(t, true, body["calls"].(map[string]any)["unlimited"])
}

func TestPortalUsage_UnlimitedLimits(t *testing.T) {
	h := NewPortalUsageHandler(&mockBudgetSvc{})
	c, w := newUsageCtx(&model.APIKey{ID: 1, TPMLimit: 0, RPMLimit: 0})
	h.GetUsage(c)
	require.Equal(t, http.StatusOK, w.Code)
	limits := parseUsageBody(t, w)["limits"].(map[string]any)
	assert.Equal(t, true, limits["tpm_unlimited"])
	assert.Equal(t, true, limits["rpm_unlimited"])
}

func TestPortalUsage_NilExpiresAt(t *testing.T) {
	h := NewPortalUsageHandler(&mockBudgetSvc{})
	c, w := newUsageCtx(&model.APIKey{ID: 1, ExpiresAt: nil})
	h.GetUsage(c)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Nil(t, parseUsageBody(t, w)["key"].(map[string]any)["expires_at"])
}
