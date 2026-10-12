package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestCodexAcceptanceInvalidGuardrailConfigIsAtomic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	if err := db.AutoMigrate(&model.SystemSetting{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.SystemSetting{Key: "guardrails_fail_open", Value: "false"}).Error; err != nil {
		t.Fatal(err)
	}
	svc := guardrail.NewGuardrailService(db, nil)
	h := NewGuardrailHandler(db, svc, nil)
	r := gin.New()
	r.PUT("/config", h.UpdateConfig)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/config", bytes.NewBufferString(`{"fail_open":true,"stream_mode":"invalid"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid config rejection, got %d", w.Code)
	}
	var st model.SystemSetting
	if err := db.First(&st, "key = ?", "guardrails_fail_open").Error; err != nil {
		t.Fatal(err)
	}
	if svc.IsFailOpen() || st.Value != "false" {
		t.Fatalf("rejected config changed safety setting: runtime=%v persisted=%q", svc.IsFailOpen(), st.Value)
	}
}
