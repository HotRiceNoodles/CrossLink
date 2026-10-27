package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestIncrementalStreamModePersistsAndReloads(t *testing.T) {
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
	svc := guardrail.NewGuardrailService(db, nil)
	h := NewGuardrailHandler(db, svc, nil)
	r := gin.New()
	r.PUT("/config", h.UpdateConfig)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/config", strings.NewReader(`{"stream_mode":"incremental"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || svc.StreamMode() != "incremental" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	reloaded := guardrail.NewGuardrailService(db, nil)
	reloaded.RefreshSettings(t.Context())
	if reloaded.StreamMode() != "incremental" {
		t.Fatal("mode was not persisted")
	}
}
