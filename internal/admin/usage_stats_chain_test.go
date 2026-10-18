package admin

// U-09 regression: Stats must build every aggregation from a FRESH query.
// The old code ran the currency probe (GROUP BY currency / ORDER BY SUM DESC
// / LIMIT 1) on the SAME *gorm.DB chain as the main stats, the rate counts
// and the currency breakdown — GORM keeps those clauses on the reused
// statement, so mixed-currency deployments got totals for ONLY the top
// currency, and the per-currency breakdown was truncated to one row by the
// leftover LIMIT. (NewDB:true was deliberately NOT used: it would drop the
// Model/tenant/time/request bindings — see the fix comment in Stats.)

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/crosslink/internal/model"
)

func newStatsTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.UsageLog{}); err != nil {
		t.Fatalf("migrate usage_logs: %v", err)
	}
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	return db
}

func seedUsageRow(t *testing.T, db *gorm.DB, org int64, currency string, cost float64, inTok, outTok, status int64, fallback int64) {
	t.Helper()
	orgID := org
	eventID := time.Now().Format("150405.000000000") + currency + string(rune('A'+org)) + string(rune('a'+inTok%26))
	row := model.UsageLog{
		EventID:        &eventID,
		OrgID:          &orgID,
		Currency:       currency,
		Cost:           cost,
		InputTokens:    int(inTok),
		OutputTokens:   int(outTok),
		StatusCode:     int(status),
		LatencyMs:      100,
		FallbackCount:  int(fallback),
		CreatedAt:      time.Now().UTC(),
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("seed row: %v", err)
	}
}

func runStats(t *testing.T, db *gorm.DB, orgID int64) map[string]any {
	t.Helper()
	h := NewUsageHandler(db, "")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/api/usage/stats", nil).WithContext(t.Context())
	// Global admin: applyTeamScope lets admins through without a team_id
	// (non-admins without a team get "1 = 0"); org scoping is what we assert.
	c.Set("role_name", "admin")
	if orgID != 0 {
		c.Set("org_id", orgID)
	}
	h.Stats(c)
	if w.Code != http.StatusOK {
		t.Fatalf("stats status %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.Data
}

func TestStats_MultiCurrencyChainIsolation(t *testing.T) {
	db := newStatsTestDB(t)

	// Org 7: CNY rows (3) with one error + one fallback, and USD rows (2)
	// whose TOTAL cost is higher (200 > 30) so the currency probe picks USD.
	seedUsageRow(t, db, 7, "CNY", 10, 100, 50, 200, 0)
	seedUsageRow(t, db, 7, "CNY", 10, 200, 60, 500, 1) // 5xx error + fallback
	seedUsageRow(t, db, 7, "CNY", 10, 300, 70, 200, 0)
	seedUsageRow(t, db, 7, "USD", 100, 400, 80, 200, 0)
	seedUsageRow(t, db, 7, "USD", 100, 500, 90, 200, 0)
	// Org 8 must not leak into org 7's stats.
	seedUsageRow(t, db, 8, "CNY", 999, 9999, 9999, 200, 9)

	data := runStats(t, db, 7)

	if got := data["currency"]; got != "USD" {
		t.Fatalf("primary currency = %v, want USD (probe must still work)", got)
	}
	// THE regression: totals must cover ALL org-7 rows, not just the top
	// currency group leaked by the old GROUP BY+LIMIT chain.
	if got := data["total_requests"]; got != float64(5) {
		t.Fatalf("total_requests = %v, want 5 (old chain-leak bug: only the top currency group was counted)", got)
	}
	if got := data["total_tokens"]; got != float64(100+50+200+60+300+70+400+80+500+90) {
		t.Fatalf("total_tokens = %v, want 1850", got)
	}
	// total_cost is primary-currency-only BY DESIGN (documented semantics).
	if got := data["total_cost"]; got != float64(200) {
		t.Fatalf("total_cost = %v, want 200 (USD primary-currency sum)", got)
	}
	// Error/fallback rates must count the CNY error row too (the old chain
	// computed rates from the top-currency group only).
	if got := data["error_rate"]; got != 0.2 {
		t.Fatalf("error_rate = %v, want 0.2 (1 error of 5 rows)", got)
	}
	if got := data["fallback_rate"]; got != 0.2 {
		t.Fatalf("fallback_rate = %v, want 0.2 (1 fallback of 5 rows)", got)
	}
	// The per-currency breakdown must list BOTH currencies (the old leftover
	// LIMIT 1 truncated it to one row).
	byCur, _ := data["cost_by_currency"].([]any)
	if len(byCur) != 2 {
		t.Fatalf("cost_by_currency has %d entries, want 2 (CNY + USD; leftover-LIMIT truncation regression)", len(byCur))
	}
}

func TestStats_EmptyScopeNoNaN(t *testing.T) {
	db := newStatsTestDB(t) // no rows at all
	data := runStats(t, db, 42)
	if got := data["total_requests"]; got != float64(0) {
		t.Fatalf("total_requests = %v, want 0", got)
	}
	if got := data["currency"]; got != "CNY" {
		t.Fatalf("default currency = %v, want CNY on empty scope", got)
	}
	for _, k := range []string{"error_rate", "cost_per_1k_tokens", "cost_per_request"} {
		if v, ok := data[k].(float64); !ok || v != 0 {
			t.Fatalf("%s = %v, want 0 (zero-division guard must hold end-to-end)", k, data[k])
		}
	}
}
