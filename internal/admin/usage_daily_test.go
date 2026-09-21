package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/model"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestDailyTrend_ScanMatchesSelect is a regression test: adding a column to
// the DailyTrend SELECT without extending the manual rows.Scan silently
// dropped EVERY row (Scan error + continue), leaving the dashboard and usage
// trend charts empty. This runs the real handler against a real (sqlite)
// database so column-count drift fails loudly.
func TestDailyTrend_ScanMatchesSelect(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.UsageLog{}); err != nil {
		t.Fatalf("migrate usage_logs: %v", err)
	}

	now := time.Now()
	logs := []model.UsageLog{
		{RouteType: "openai", ModelRequested: "m", ModelUsed: "m", Currency: "CNY", StatusCode: 200, CreatedAt: now.Add(-2 * 24 * time.Hour)},
		{RouteType: "openai", ModelRequested: "m", ModelUsed: "m", Currency: "CNY", StatusCode: 500, CreatedAt: now.Add(-1 * 24 * time.Hour)},
		{RouteType: "openai", ModelRequested: "m", ModelUsed: "m", Currency: "CNY", StatusCode: 200, CreatedAt: now},
	}
	if err := db.Create(&logs).Error; err != nil {
		t.Fatalf("seed usage_logs: %v", err)
	}

	h := NewUsageHandler(db, "")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/daily?days=7", nil)
	setAdminContext(c, 1, 1, "admin") // super admin: no team/org scoping (1=0 otherwise)
	h.DailyTrend(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data []DailyStat `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Data) == 0 {
		t.Fatal("daily trend returned no rows — rows.Scan no longer matches the SELECT column list")
	}

	var total int64
	var errors int64
	for _, r := range resp.Data {
		total += r.Count
		errors += r.ErrorCountDaily
	}
	if total != 3 {
		t.Fatalf("total count = %d, want 3 (all rows must survive the scan)", total)
	}
	if errors != 1 {
		t.Fatalf("error_count_daily total = %d, want 1", errors)
	}
}
