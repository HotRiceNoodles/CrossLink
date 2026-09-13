package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// pgStreamProvider emits a fixed, pre-buffered sequence of SSE chunks.
type pgStreamProvider struct {
	name string
}

func (p *pgStreamProvider) Chat(context.Context, *domain.OpenAIRequest, string) (*domain.OpenAIResponse, error) {
	return nil, nil
}

func (p *pgStreamProvider) StreamChat(_ context.Context, _ *domain.OpenAIRequest, _ string) (<-chan domain.SSEChunk, error) {
	ch := make(chan domain.SSEChunk, 3)
	ch <- domain.SSEChunk{Chunk: &domain.OpenAIChunk{Choices: []domain.OpenAIChunkChoice{{
		Delta: domain.OpenAIChunkDelta{Content: "hello"},
	}}}}
	ch <- domain.SSEChunk{Chunk: &domain.OpenAIChunk{Usage: &domain.OpenAIChunkUsage{
		PromptTokens: 10, CompletionTokens: 5,
	}}}
	ch <- domain.SSEChunk{Done: true}
	close(ch)
	return ch, nil
}

func (p *pgStreamProvider) Name() string { return p.name }

// pgModelRepo implements router.ProviderModelRepo for playground tests.
type pgModelRepo struct{}

func (m *pgModelRepo) FindByModelName(_ context.Context, name string, _ int64) ([]model.ProviderModel, error) {
	if name != "test-model" {
		return nil, nil
	}
	return []model.ProviderModel{{
		ID:            1,
		ModelName:     name,
		ProviderModel: "fake-model",
		Weight:        100,
		Priority:      1,
		Status:        1,
		Provider:      model.Provider{Name: "fake", Status: 1},
	}}, nil
}

// TestPlaygroundStreamChat_ClientDisconnect_StillRecordsUsage is a regression
// test: the client-disconnect path in StreamChat used to `return` before
// logUsage, so playground calls that were stopped mid-stream (upstream tokens
// already consumed) never appeared in usage_logs — dashboard call counts
// missed them. The fix breaks out of the stream loop and falls through to
// logUsage. It also asserts OrgID is stamped so org-scoped dashboards count
// playground rows.
func TestPlaygroundStreamChat_ClientDisconnect_StillRecordsUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.UsageLog{}); err != nil {
		t.Fatalf("migrate usage_logs: %v", err)
	}
	// :memory: gives every pooled connection its own database — pin one
	// connection or the async logUsage INSERT lands in a different (empty) DB.
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}

	reg := provider.NewRegistry()
	reg.Register("fake", &pgStreamProvider{name: "fake"})
	resolver := router.NewResolver(reg, &pgModelRepo{}, nil, map[router.StrategyName]router.RoutingStrategy{
		router.StrategyWeightedRandom: &router.WeightedRandomStrategy{},
	}, nil, nil, nil, nil)

	h := NewPlaygroundHandler(resolver, service.NewUsageService(repository.NewUsageLogRepo(db)),
		nil, nil, nil, nil, nil)

	// Request context already cancelled = client disconnected mid-stream.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/api/playground/stream", strings.NewReader(
		`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`,
	)).WithContext(ctx)
	c.Set("org_id", int64(7))

	h.StreamChat(c)

	// logUsage runs in a goroutine — poll briefly for the usage row.
	deadline := time.Now().Add(2 * time.Second)
	for {
		var logs []model.UsageLog
		db.Where("route_type = ?", "playground").Find(&logs)
		if len(logs) == 1 {
			if logs[0].OrgID == nil || *logs[0].OrgID != 7 {
				t.Fatalf("usage row org_id = %v, want 7 (org-scoped dashboards filter NULL rows)", logs[0].OrgID)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("client-disconnected playground stream was not recorded in usage_logs")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
