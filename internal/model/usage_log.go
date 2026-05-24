package model

import (
	"time"

	"gorm.io/datatypes"
)

type UsageLog struct {
	ID               int64      `gorm:"primaryKey" json:"id"`
	RequestID        string     `gorm:"size:64;not null" json:"request_id"`
	APIKeyID         *int64     `gorm:"index" json:"api_key_id"`
	ProviderID       *int64     `gorm:"index" json:"provider_id"`
	RouteType        string     `gorm:"size:16;not null" json:"route_type"`
	ModelRequested   string     `gorm:"size:128;not null;index" json:"model_requested"`
	ModelUsed        string     `gorm:"size:128;not null" json:"model_used"`
	InputTokens      int        `gorm:"not null;default:0" json:"input_tokens"`
	OutputTokens     int        `gorm:"not null;default:0" json:"output_tokens"`
	Cost             float64    `gorm:"type:decimal(16,8);not null;default:0" json:"cost"`
	LatencyMs        int        `gorm:"not null;default:0" json:"latency_ms"`
	FirstTokenMs     *int       `json:"first_token_ms"`
	StatusCode       int        `gorm:"not null" json:"status_code"`
	ErrorType        string     `gorm:"size:64" json:"error_type"`
	Currency         string     `gorm:"size:3;not null;default:'CNY'" json:"currency"`
	TeamID           *int64     `gorm:"index" json:"team_id"`
	UserMessage      *string    `gorm:"type:text" json:"user_message,omitempty"`
	ModelResponse    *string    `gorm:"type:text" json:"model_response,omitempty"`
	FallbackCount      int        `gorm:"not null;default:0" json:"fallback_count"`
	RetryCount         int        `gorm:"not null;default:0" json:"retry_count"`
	GuardrailTriggered bool       `gorm:"not null;default:false" json:"guardrail_triggered"`
	GuardrailRule      string     `gorm:"size:255" json:"guardrail_rule,omitempty"`
	CacheHit           bool       `gorm:"not null;default:false" json:"cache_hit"`
	AgentType          string     `gorm:"size:32" json:"agent_type,omitempty"`
	SecurityEvents     datatypes.JSON `gorm:"type:jsonb;default:'[]'" json:"security_events,omitempty"`
	CreatedAt          time.Time  `gorm:"not null;default:now();index" json:"created_at"`
}

func (UsageLog) TableName() string { return "usage_logs" }

func ValidCurrency(c string) string {
	if c == "USD" {
		return "USD"
	}
	return "CNY"
}
