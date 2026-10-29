package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type gatewayPhoneProvider struct{}

func (*gatewayPhoneProvider) Name() string { return "phone-fixture" }
func (*gatewayPhoneProvider) Chat(context.Context, *domain.OpenAIRequest, string) (*domain.OpenAIResponse, error) {
	return &domain.OpenAIResponse{Choices: []domain.OpenAIChoice{{Message: domain.OpenAIMessage{Role: "assistant", Content: "安全前文【虚构手机号：13800138000】后文"}}}}, nil
}
func (*gatewayPhoneProvider) StreamChat(context.Context, *domain.OpenAIRequest, string) (<-chan domain.SSEChunk, error) {
	ch := make(chan domain.SSEChunk, 5)
	for _, text := range []string{"安全前文【虚构手机号：138", "0013", "8000】", "后文"} {
		ch <- domain.SSEChunk{Chunk: &domain.OpenAIChunk{Choices: []domain.OpenAIChunkChoice{{Index: 0, Delta: domain.OpenAIChunkDelta{Content: text}}}}}
	}
	ch <- domain.SSEChunk{Done: true}
	close(ch)
	return ch, nil
}

type gatewayPhoneModels struct{}

func (*gatewayPhoneModels) FindByModelName(context.Context, string, int64) ([]model.ProviderModel, error) {
	return []model.ProviderModel{{ID: 1, ModelName: "phone-test", ProviderModel: "phone-test", Weight: 1, Status: 1,
		Provider: model.Provider{Name: "phone-fixture", Status: 1}}}, nil
}

func TestGatewayWiringEnforcesOrganizationResponseRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			name := path + "/json"
			if stream {
				name = path + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
				if err != nil {
					t.Fatal(err)
				}
				sqlDB, _ := db.DB()
				sqlDB.SetMaxOpenConns(1)
				defer sqlDB.Close()
				if err := db.AutoMigrate(&model.SystemSetting{}, &model.UsageLog{}); err != nil {
					t.Fatal(err)
				}
				if err := db.Exec("CREATE TABLE guardrail_rules (id INTEGER PRIMARY KEY, org_id INTEGER, name TEXT, type TEXT, direction TEXT, enabled BOOLEAN, config TEXT, severity TEXT, action TEXT, model_filter TEXT, version INTEGER, created_at DATETIME, updated_at DATETIME)").Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Exec("CREATE TABLE provider_models (model_name TEXT, extra_config TEXT)").Error; err != nil {
					t.Fatal(err)
				}
				for key, value := range map[string]string{"guardrails_enabled": "true", "guardrails_stream_mode": "incremental"} {
					if err := db.Create(&model.SystemSetting{Key: key, Value: value}).Error; err != nil {
						t.Fatal(err)
					}
				}
				orgID := int64(7)
				rule := guardrail.GuardrailRule{ID: 1, OrgID: &orgID, Name: "组织手机号阻断", Enabled: true,
					Type: "keyword_filter", Direction: "response", Action: "block", Severity: "high", Version: 1,
					Config: datatypes.JSON(`{"blocklist":["1[3-9][0-9]{9}"],"use_regex":true}`)}
				if err := db.Create(&rule).Error; err != nil {
					t.Fatal(err)
				}
				gs := guardrail.NewGuardrailService(db, nil)
				gs.RefreshSettings(t.Context())
				registry := provider.NewRegistry()
				registry.Register("phone-fixture", &gatewayPhoneProvider{})
				resolver := router.NewResolver(registry, &gatewayPhoneModels{}, nil,
					map[router.StrategyName]router.RoutingStrategy{router.StrategyWeightedRandom: &router.WeightedRandomStrategy{}}, nil, nil, nil, nil)
				svcs := &service.Services{UsageSvc: service.NewUsageService(repository.NewUsageLogRepo(db)), ActiveTracker: service.NewActiveRequestTracker(nil)}
				infra := &Infrastructure{Resolver: resolver, Registry: registry,
					GatewaySvc: service.NewGatewayService(resolver, registry, nil, nil, nil)}
				openai, anthropic := newGatewayProtocolHandlers(infra, svcs, gs)
				r := gin.New()
				r.Use(func(c *gin.Context) { c.Set("org_id", orgID) }, middleware.GuardrailsRequest(gs, nil))
				r.POST("/v1/chat/completions", openai.HandleChatCompletions)
				r.POST("/v1/messages", anthropic.HandleMessages)
				body := `{"model":"phone-test","messages":[{"role":"user","content":"请输出测试模板"}],"max_tokens":2000`
				if stream {
					body += `,"stream":true`
				}
				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body+`}`))
				req.Header.Set("Content-Type", "application/json")
				r.ServeHTTP(w, req)
				for deadline := time.Now().Add(200 * time.Millisecond); time.Now().Before(deadline); {
					var count int64
					db.Model(&model.UsageLog{}).Count(&count)
					if count > 0 {
						break
					}
					time.Sleep(2 * time.Millisecond)
				}
				if !strings.Contains(w.Body.String(), "guardrail_blocked") || strings.Contains(w.Body.String(), "后文") {
					t.Fatalf("正式处理器未执行响应阻断：%d %s", w.Code, w.Body.String())
				}
				if stream && w.Header().Get("X-Guardrail-Stream-Mode") != "strict" {
					t.Fatal("不支持增量的强制规则未回退严格审核")
				}
			})
		}
	}
}
