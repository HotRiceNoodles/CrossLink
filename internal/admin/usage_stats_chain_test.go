package admin

// U-09 regression (single-currency edition): Stats must build every
// aggregation from a FRESH query. The historic bug ran a currency probe
// (GROUP BY currency / ORDER BY SUM DESC / LIMIT 1) on the SAME *gorm.DB
// chain as the main stats and the rate counts — GORM keeps those clauses on
// the reused statement, so totals covered only the top currency group. The
// probe itself is GONE under the single-currency design (the deployment's
// billing_currency is fixed at first install and every row carries it), but
// the chain-isolation property is still asserted here: totals, rates and
// cost each aggregate independently and org scoping never leaks.

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
	"github.com/crosslink/internal/settings"
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
		EventID:       &eventID,
		OrgID:         &orgID,
		Currency:      currency,
		Cost:          cost,
		InputTokens:   int(inTok),
		OutputTokens:  int(outTok),
		StatusCode:    int(status),
		LatencyMs:     100,
		FallbackCount: int(fallback),
		CreatedAt:     time.Now().UTC(),
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("seed row: %v", err)
	}
}

func runStats(t *testing.T, db *gorm.DB, orgID int64) map[string]any {
	t.Helper()
	h := NewUsageHandler(db, "", nil)
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

func TestStats_SingleCurrencyChainIsolation(t *testing.T) {
	t.Parallel()
	db := newStatsTestDB(t)

	// Org 7: five rows, one error + one fallback. (Historic seeds mixed CNY
	// and USD to exercise the probe; under single-currency the row currency
	// is lineage only — the currency label comes from the platform setting.)
	seedUsageRow(t, db, 7, "CNY", 10, 100, 50, 200, 0)
	seedUsageRow(t, db, 7, "CNY", 10, 200, 60, 500, 1) // 5xx error + fallback
	seedUsageRow(t, db, 7, "CNY", 10, 300, 70, 200, 0)
	seedUsageRow(t, db, 7, "CNY", 100, 400, 80, 200, 0)
	seedUsageRow(t, db, 7, "CNY", 100, 500, 90, 200, 0)
	// Org 8 must not leak into org 7's stats.
	seedUsageRow(t, db, 8, "CNY", 999, 9999, 9999, 200, 9)

	data := runStats(t, db, 7)

	// The currency echoed is the deployment's fixed billing currency, never
	// derived from the rows.
	if got := data["currency"]; got != settings.PlatformCurrency() {
		t.Fatalf("currency = %v, want platform currency %s", got, settings.PlatformCurrency())
	}
	// Chain isolation: totals cover ALL org-7 rows (the historic chain-leak
	// bug counted only the top currency group).
	if got := data["total_requests"]; got != float64(5) {
		t.Fatalf("total_requests = %v, want 5 (chain-leak regression: a clipped group)", got)
	}
	if got := data["total_tokens"]; got != float64(100+50+200+60+300+70+400+80+500+90) {
		t.Fatalf("total_tokens = %v, want 1850", got)
	}
	// Single-currency semantics: cost sums every row — no per-currency
	// filtering, no cross-currency ambiguity.
	if got := data["total_cost"]; got != float64(230) {
		t.Fatalf("total_cost = %v, want 230 (sum of all rows)", got)
	}
	if got := data["error_rate"]; got != 0.2 {
		t.Fatalf("error_rate = %v, want 0.2 (1 error of 5 rows)", got)
	}
	if got := data["fallback_rate"]; got != 0.2 {
		t.Fatalf("fallback_rate = %v, want 0.2 (1 fallback of 5 rows)", got)
	}
	// The per-currency breakdown is gone by design.
	if _, present := data["cost_by_currency"]; present {
		t.Fatal("cost_by_currency must not be present (removed under single-currency design)")
	}
}

func TestStats_EmptyScopeNoNaN(t *testing.T) {
	t.Parallel()
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
