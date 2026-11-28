package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 矩阵 G-R1 审计修复的回归测试:/v1/chat/completions 的 handler 自有闭包
// 此前从不应用 request_overrides(只有 GatewayService/messages 路径应用),
// 管理员模型级参数注入在 openai 入站上被静默忽略。

type overrideCapturingProvider struct{ got *domain.OpenAIRequest }

func (p *overrideCapturingProvider) Chat(_ context.Context, req *domain.OpenAIRequest, _ string) (*domain.OpenAIResponse, error) {
	cp := *req
	p.got = &cp
	return &domain.OpenAIResponse{Choices: []domain.OpenAIChoice{{Message: domain.OpenAIMessage{Role: "assistant", Content: "ok"}}}}, nil
}
func (p *overrideCapturingProvider) StreamChat(_ context.Context, _ *domain.OpenAIRequest, _ string) (<-chan domain.SSEChunk, error) {
	return nil, nil
}
func (p *overrideCapturingProvider) Name() string { return "override-cap" }

var _ provider.Provider = (*overrideCapturingProvider)(nil)

func TestOpenAIChatClosure_AppliesRouteRequestOverrides(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.UsageLog{}))

	p := &overrideCapturingProvider{}
	route := &router.RouteResult{
		Provider: p, ProviderModel: "synthetic", ProviderRow: &model.Provider{},
		RequestOverrides: map[string]any{"temperature": 0.42, "enable_thinking": true},
	}
	h := &OpenAIHandler{usageSvc: service.NewUsageService(repository.NewUsageLogRepo(db))}
	req := &domain.OpenAIRequest{Model: "synthetic", Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	h.handleNonStream(c, []*router.RouteResult{route}, req, nil, 0, time.Now(), "")

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	require.NotNil(t, p.got, "provider was not called")
	require.NotNil(t, p.got.Temperature)
	assert.InDelta(t, 0.42, *p.got.Temperature, 1e-9, "admin temperature override must reach the provider on the openai-chat closure")
	require.NotNil(t, p.got.Extensions, "dialect override must create the envelope on the candidate copy")
	assert.Contains(t, p.got.Extensions.Params, "dialect.enable_thinking")
	// M3 §1.3 clone proof: the candidate's envelope mutation must not leak
	// back into the caller's request object.
	assert.Nil(t, req.Extensions, "original request envelope must stay untouched")
}
