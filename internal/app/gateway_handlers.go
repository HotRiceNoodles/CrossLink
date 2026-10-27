package app

import (
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/handler"
	"github.com/crosslink/internal/service"
)

// newGatewayProtocolHandlers 集中构造正式路由使用的协议处理器，便于验证启动接线。
func newGatewayProtocolHandlers(infra *Infrastructure, svcs *service.Services, guardrailSvc *guardrail.GuardrailService) (*handler.OpenAIHandler, *handler.AnthropicHandler) {
	// 增量中间件可能回退严格模式；处理器必须持有同一服务，才能完成响应审核。
	openai := handler.NewOpenAIHandler(infra.Resolver, svcs.UsageSvc, svcs.LatencySvc, nil, svcs.ActiveTracker, svcs.IdemCache, infra.RetryBudget, guardrailSvc)
	anthropic := handler.NewAnthropicHandler(infra.GatewaySvc, infra.Resolver, svcs.UsageSvc, svcs.IdemCache, guardrailSvc)
	return openai, anthropic
}
