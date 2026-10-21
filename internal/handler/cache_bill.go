package handler

import (
	"net/http"

	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

// NewCacheHitBiller bills semantic-cache HITs (r63 probe defect). The Cache
// middleware serves the cached body and aborts before any handler runs, so
// the normal handler-side usage submit never fires; without this biller a
// HIT was a served 200 with no ledger row, no budget settlement and no
// admission enforcement. The biller runs on the HIT path and goes through
// submitUsageEntryFunc — WAL-first, emergency-overflow and exactly one
// durable row per served request — with CacheHit=true so analytics can tell
// cached servings from upstream ones. Budget reservation/settlement is NOT
// done here: BudgetCheck/ReportBudgetUsage now wrap the Cache middleware and
// settle from the context tokens+prices the HIT path restores.
func NewCacheHitBiller(log usageEntryLogger) middleware.CacheHitBiller {
	return func(c *gin.Context, cached *service.CacheEntry) {
		if cached == nil {
			return
		}
		var apiKeyID, teamID int64
		orgID := c.GetInt64("org_id")
		if key := middleware.GetAPIKeyFromContext(c); key != nil {
			apiKeyID = key.ID
			if key.TeamID != nil {
				teamID = *key.TeamID
			}
		}
		latencyMs := int64(0)
		if v, ok := c.Get("cache_hit_latency_ms"); ok {
			latencyMs, _ = v.(int64)
		}
		currency := cached.Currency
		if currency == "" {
			currency = "CNY"
		}
		entry := &service.UsageEntry{
			GinRequestID:   c.GetString("request_id"),
			RouteType:      cacheBillRouteType(c.FullPath()),
			ModelRequested: cached.Model,
			ModelUsed:      cached.Model,
			InputTokens:    cached.InputTokens,
			OutputTokens:   cached.OutputTokens,
			InputPrice:     cached.InputPrice,
			OutputPrice:    cached.OutputPrice,
			Currency:       currency,
			StatusCode:     http.StatusOK,
			LatencyMs:      latencyMs,
			APIKeyID:       apiKeyID,
			TeamID:         teamID,
			OrgID:          orgID,
			CacheHit:       true,
			SessionID:      c.GetHeader("X-Session-ID"),
			TemplateID:     readTemplateID(c),
		}
		submitUsageEntryFunc(log, entry, func() {
			if v, ok := c.Get("guardrail_triggered"); ok {
				if b, _ := v.(bool); b {
					entry.GuardrailTriggered = true
				}
			}
			if v, ok := c.Get("guardrail_rule"); ok {
				if s, _ := v.(string); s != "" {
					entry.GuardrailRule = s
				}
			}
			if v, ok := c.Get("guardrail_truncated"); ok {
				if b, _ := v.(bool); b {
					entry.GuardrailTruncated = true
				}
			}
			if v, ok := c.Get("agent_type"); ok {
				if s, _ := v.(string); s != "" {
					entry.AgentType = s
				}
			}
			if v, ok := c.Get("security_events"); ok {
				if d, _ := v.(datatypes.JSON); d != nil {
					entry.SecurityEvents = d
				}
			}
		})
	}
}

func cacheBillRouteType(pattern string) string {
	switch pattern {
	case "/v1/messages":
		return "anthropic"
	case "/v1/chat/completions":
		return "openai"
	case "/v1/embeddings":
		return "embeddings"
	default:
		return "unknown"
	}
}
