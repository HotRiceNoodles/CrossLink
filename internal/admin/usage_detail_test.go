package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newUsageTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.UsageLog{}); err != nil {
		t.Fatalf("migrate usage_logs: %v", err)
	}
	return db
}

func seedUsageLog(t *testing.T, db *gorm.DB, mutate func(*model.UsageLog)) {
	t.Helper()
	msg := "hello"
	resp := "world"
	log := model.UsageLog{
		RequestID:       "abcd1234",
		RouteType:       "anthropic",
		ModelRequested:  "m",
		ModelUsed:       "m",
		InputTokens:     10,
		OutputTokens:    20,
		Cost:            0.5,
		BillableCost:    0.75,
		LatencyMs:       120,
		StatusCode:      200,
		Currency:        "CNY",
		SecurityEvents:  datatypes.JSON(`[]`),
		ContextSnapshot: datatypes.JSON(`{"buckets":{"system_tokens":1}}`),
		UserMessage:     &msg,
		ModelResponse:   &resp,
		CreatedAt:       time.Now(),
	}
	if mutate != nil {
		mutate(&log)
	}
	if err := db.Create(&log).Error; err != nil {
		t.Fatalf("seed usage_logs: %v", err)
	}
}

func TestUsageGet_ReturnsFullRow(t *testing.T) {
	db := newUsageTestDB(t)
	seedUsageLog(t, db, nil)

	h := NewUsageHandler(db, "")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/1", nil)
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "id", Value: "1"}})
	h.Get(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data map[string]any `json:"data"`
	}
	decodeResponse(t, w, &resp)
	if resp.Data["user_message"] != "hello" {
		t.Fatalf("user_message = %v, want hello (detail must include content columns)", resp.Data["user_message"])
	}
	if resp.Data["model_response"] != "world" {
		t.Fatalf("model_response = %v, want world", resp.Data["model_response"])
	}
	if resp.Data["request_id"] != "abcd1234" {
		t.Fatalf("request_id = %v, want abcd1234", resp.Data["request_id"])
	}
	if _, ok := resp.Data["billable_cost"]; !ok {
		t.Fatal("billable_cost missing from detail response")
	}
}

func TestUsageGet_NotFound(t *testing.T) {
	db := newUsageTestDB(t)

	h := NewUsageHandler(db, "")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/99999", nil)
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "id", Value: "99999"}})
	h.Get(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	var resp struct {
		ErrorCode string `json:"error_code"`
	}
	decodeResponse(t, w, &resp)
	if resp.ErrorCode != "not_found" {
		t.Fatalf("error_code = %q, want not_found", resp.ErrorCode)
	}
}

func TestUsageGet_InvalidID(t *testing.T) {
	db := newUsageTestDB(t)

	h := NewUsageHandler(db, "")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/abc", nil)
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "id", Value: "abc"}})
	h.Get(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestUsageGet_TeamScopeMismatch(t *testing.T) {
	db := newUsageTestDB(t)
	teamID := int64(8)
	seedUsageLog(t, db, func(l *model.UsageLog) { l.TeamID = &teamID })

	h := NewUsageHandler(db, "")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/1", nil)
	setAdminContext(c, 2, 5, "viewer")
	c.Set("team_id", int64(7)) // different team: must not see the row
	setPathParams(c, gin.Params{{Key: "id", Value: "1"}})
	h.Get(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (scoped row must be invisible, not 403)", w.Code)
	}
}

func TestUsageGet_OrgScopeMismatch(t *testing.T) {
	db := newUsageTestDB(t)
	orgID := int64(1)
	seedUsageLog(t, db, func(l *model.UsageLog) { l.OrgID = &orgID })

	h := NewUsageHandler(db, "")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/1", nil)
	setAdminContext(c, 1, 1, "admin")
	c.Set("org_id", int64(99)) // different org: must not see the row
	setPathParams(c, gin.Params{{Key: "id", Value: "1"}})
	h.Get(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (org-scoped row must be invisible)", w.Code)
	}
}

// TestUsageList_OmitsContentColumns is a regression test: List must not ship
// the heavy TEXT content columns (user_message, model_response, up to 64KB
// each) in every list row; full rows are served by Get. Decoding into maps
// (not the model struct) is what makes "key absent" assertable.
func TestUsageList_OmitsContentColumns(t *testing.T) {
	db := newUsageTestDB(t)
	seedUsageLog(t, db, nil)
	seedUsageLog(t, db, func(l *model.UsageLog) { l.RequestID = "efgh5678" })

	h := NewUsageHandler(db, "")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage?page=1&page_size=20", nil)
	setAdminContext(c, 1, 1, "admin")
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	decodeResponse(t, w, &resp)
	if len(resp.Data) != 2 {
		t.Fatalf("rows = %d, want 2", len(resp.Data))
	}
	for i, row := range resp.Data {
		for _, col := range []string{"user_message", "model_response"} {
			if _, ok := row[col]; ok {
				t.Fatalf("row %d: %q must be absent from list responses", i, col)
			}
		}
		for _, col := range []string{"request_id", "cost", "context_snapshot", "billable_cost", "reasoning_tokens"} {
			if _, ok := row[col]; !ok {
				t.Fatalf("row %d: %q missing from list response — usageListColumns drifted from the model?", i, col)
			}
		}
	}
}
