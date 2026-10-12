package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestCodexAcceptanceOpenAIHandlerStrictBlocksShortSplitMarker(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	defer sqlDB.Close()
	if err := db.AutoMigrate(&model.UsageLog{}); err != nil {
		t.Fatal(err)
	}
	statements := []string{
		"CREATE TABLE system_settings (key TEXT, value TEXT)",
		"INSERT INTO system_settings VALUES ('guardrails_enabled','true'),('guardrails_stream_mode','strict')",
		"CREATE TABLE guardrail_rules (id INTEGER PRIMARY KEY, name TEXT, type TEXT, direction TEXT, enabled BOOLEAN, config TEXT, severity TEXT, action TEXT)",
		`INSERT INTO guardrail_rules VALUES (1,'synthetic','keyword_filter','response',true,'{"blocklist":["SECRET"]}','high','block')`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	gs := guardrail.NewGuardrailService(db, nil)
	gs.RefreshSettings(t.Context())
	h := &OpenAIHandler{guardrailSvc: gs, usageSvc: service.NewUsageService(repository.NewUsageLogRepo(db))}
	chunk := func(index int, text string) domain.SSEChunk {
		return domain.SSEChunk{Chunk: &domain.OpenAIChunk{Choices: []domain.OpenAIChunkChoice{{Index: index, Delta: domain.OpenAIChunkDelta{Content: text}}}}}
	}
	p := &fakeStreamProvider{name: "acceptance", chunks: []domain.SSEChunk{chunk(0, "SE"), chunk(1, "other"), chunk(0, "CRET"), {Done: true}}}
	routes := []*router.RouteResult{{Provider: p, ProviderModel: "synthetic", ProviderRow: &model.Provider{}}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	h.handleStream(c, routes, &domain.OpenAIRequest{Stream: true, Model: "synthetic", Messages: []domain.OpenAIMessage{{Role: "user", Content: "hello"}}}, nil, 0, time.Now(), "acceptance")
	for _, original := range []string{`"content":"SE"`, `"content":"CRET"`} {
		if strings.Contains(w.Body.String(), original) {
			t.Fatalf("strict handler emitted unchecked content: %s", w.Body.String())
		}
	}
	if !strings.Contains(w.Body.String(), "guardrail_blocked") {
		t.Fatalf("actual handler did not block: %s", w.Body.String())
	}
}
