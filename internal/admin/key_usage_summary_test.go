package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupKeyUsageDB(t *testing.T) *gorm.DB {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.UsageLog{}); err != nil {
		t.Fatalf("migrate usage_logs: %v", err)
	}
	return db
}

func callKeyUsageSummary(t *testing.T, h *UsageHandler, orgID int64) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/api/keys/usage-summary", nil)
	c.Set("org_id", orgID)
	// Admin role so applyTeamScope does not zero out the query (team scoping
	// is exercised by the existing usage-list scope tests).
	c.Set("role_name", model.RoleAdmin)
	h.KeyUsageSummary(c)
	return w, c
}

// TestKeyUsageSummary_MonthWindowAndOrgScope: the endpoint aggregates the
// current calendar month per api_key_id using billable_cost (the same billing
// basis as reconciliation), excludes other months, ignores key-less rows and
// is org-scoped.
func TestKeyUsageSummary_MonthWindowAndOrgScope(t *testing.T) {
	db := setupKeyUsageDB(t)
	h := NewUsageHandler(db, "", nil)

	org7, org9 := int64(7), int64(9)
	key101, key102, key103 := int64(101), int64(102), int64(103)
	now := time.Now()

	seed := []model.UsageLog{
		{APIKeyID: &key101, OrgID: &org7, BillableCost: 10.5, CreatedAt: now},
		{APIKeyID: &key101, OrgID: &org7, BillableCost: 2.5, CreatedAt: now},
		// Same key, last month → excluded by the calendar-month window.
		{APIKeyID: &key101, OrgID: &org7, BillableCost: 99, CreatedAt: now.AddDate(0, -1, 0)},
		// Another key, same month.
		{APIKeyID: &key102, OrgID: &org7, BillableCost: 4, CreatedAt: now},
		// Other org → excluded by org scope.
		{APIKeyID: &key103, OrgID: &org9, BillableCost: 8, CreatedAt: now},
		// Key-less rows (playground/dashboard calls) → NULL api_key_id, ignored.
		{OrgID: &org7, BillableCost: 50, CreatedAt: now},
	}
	for i := range seed {
		if err := db.Create(&seed[i]).Error; err != nil {
			t.Fatalf("seed usage row %d: %v", i, err)
		}
	}

	w, _ := callKeyUsageSummary(t, h, org7)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// key 101: 2 rows this month (both today), billable 13
	for _, sub := range []string{`"api_key_id":101`, `"month_requests":2`, `"month_cost":13`} {
		if !strings.Contains(body, sub) {
			t.Fatalf("key 101 aggregation missing %s: %s", sub, body)
		}
	}
	// All seeded rows are "now" → day and week windows match the month window
	// (three-window pairing for per-key budget_period limits).
	if !strings.Contains(body, `"day_requests":2`) || !strings.Contains(body, `"day_cost":13`) {
		t.Fatalf("key 101 day window wrong: %s", body)
	}
	if !strings.Contains(body, `"week_requests":2`) || !strings.Contains(body, `"week_cost":13`) {
		t.Fatalf("key 101 week window wrong: %s", body)
	}
	// key 102: 1 row, 4
	if !strings.Contains(body, `"api_key_id":102`) || !strings.Contains(body, `"month_cost":4`) {
		t.Fatalf("key 102 aggregation wrong: %s", body)
	}
	// Other-org key and key-less rows must not leak.
	if strings.Contains(body, `"api_key_id":103`) || strings.Contains(body, `"api_key_id":0`) {
		t.Fatalf("org/key scope leaked: %s", body)
	}
	// Last month's 99 must not appear anywhere.
	if strings.Contains(body, `"month_cost":99`) {
		t.Fatalf("last-month row leaked into month window: %s", body)
	}
}

func TestKeyUsageSummary_EmptyReturnsArray(t *testing.T) {
	db := setupKeyUsageDB(t)
	h := NewUsageHandler(db, "", nil)
	w, _ := callKeyUsageSummary(t, h, 7)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"data":[]`) {
		t.Fatalf("empty org should return an empty array, got: %s", w.Body.String())
	}
}
