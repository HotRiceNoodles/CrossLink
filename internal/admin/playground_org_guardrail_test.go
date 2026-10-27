package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
)

type pgOrgPhoneProvider struct{}

func (*pgOrgPhoneProvider) Name() string { return "fake" }
func (*pgOrgPhoneProvider) Chat(context.Context, *domain.OpenAIRequest, string) (*domain.OpenAIResponse, error) {
	return &domain.OpenAIResponse{Choices: []domain.OpenAIChoice{{Message: domain.OpenAIMessage{Role: "assistant", Content: "【虚构手机号：13800138000】后文"}}}}, nil
}
func (*pgOrgPhoneProvider) StreamChat(context.Context, *domain.OpenAIRequest, string) (<-chan domain.SSEChunk, error) {
	ch := make(chan domain.SSEChunk, 4)
	for _, text := range []string{"【虚构手机号：138", "0013", "8000】后文"} {
		ch <- domain.SSEChunk{Chunk: &domain.OpenAIChunk{Choices: []domain.OpenAIChunkChoice{{Index: 0, Delta: domain.OpenAIChunkDelta{Content: text}}}}}
	}
	ch <- domain.SSEChunk{Done: true}
	close(ch)
	return ch, nil
}

func TestPlaygroundOrganizationResponseRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, surface := range []string{"chat", "stream", "responses-json", "responses-stream", "responses-native"} {
		for _, orgID := range []int64{7, 8} {
			t.Run(fmt.Sprintf("%s/org%d", surface, orgID), func(t *testing.T) {
				db, gs := pgRespGuardSetup(t, "incremental")
				for _, statement := range []string{
					"ALTER TABLE guardrail_rules ADD COLUMN org_id INTEGER",
					`UPDATE guardrail_rules SET org_id=7, config='{"blocklist":["1[3-9][0-9]{9}"],"use_regex":true}'`,
					"CREATE TABLE provider_models (model_name TEXT, extra_config TEXT)",
				} {
					if err := db.Exec(statement).Error; err != nil {
						t.Fatal(err)
					}
				}
				registry := provider.NewRegistry()
				registry.Register("fake", &pgOrgPhoneProvider{})
				resolver := router.NewResolver(registry, &pgModelRepo{}, nil,
					map[router.StrategyName]router.RoutingStrategy{router.StrategyWeightedRandom: &router.WeightedRandomStrategy{}}, nil, nil, nil, nil)
				h := NewPlaygroundHandler(resolver, service.NewUsageService(repository.NewUsageLogRepo(db)), nil, nil, nil, gs, nil)
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Set("org_id", orgID)
				body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"input":"hi"`
				if surface == "stream" || surface == "responses-stream" {
					body += `,"stream":true`
				}
				c.Request = httptest.NewRequest(http.MethodPost, "/admin/api/playground/"+surface, strings.NewReader(body+`}`))
				c.Request.Header.Set("Content-Type", "application/json")
				switch surface {
				case "chat":
					h.Chat(c)
				case "stream":
					h.StreamChat(c)
				case "responses-native":
					pgCopySSEGuarded(c, strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"138\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"00138000后文\"}\n\ndata: [DONE]\n\n"), gs, "test-model")
				default:
					h.Responses(c)
				}
				if surface != "responses-native" {
					for deadline := time.Now().Add(200 * time.Millisecond); time.Now().Before(deadline); {
						var count int64
						db.Model(&model.UsageLog{}).Count(&count)
						if count > 0 {
							break
						}
						time.Sleep(2 * time.Millisecond)
					}
				}
				blocked := strings.Contains(w.Body.String(), "guardrail_blocked")
				if blocked != (orgID == 7) {
					t.Fatalf("组织响应规则未正确执行：%d %s", w.Code, w.Body.String())
				}
				if orgID == 7 && strings.Contains(w.Body.String(), "后文") {
					t.Fatal("命中后的内容被放行")
				}
			})
		}
	}
}
