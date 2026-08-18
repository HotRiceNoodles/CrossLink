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
	BillableCost     float64    `gorm:"type:decimal(16,8);not null;default:0" json:"billable_cost"` // upstream cost × key price_multiplier
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
	SecurityEvents     datatypes.JSON `gorm:"default:'[]'" json:"security_events,omitempty"`
	OrgID              *int64     `gorm:"index" json:"org_id"`
	ReasoningTokens    int        `gorm:"default:0" json:"reasoning_tokens"`
	CacheReadTokens    int        `gorm:"default:0" json:"cache_read_tokens"`
	SessionID          string     `gorm:"size:255;index" json:"session_id,omitempty"`
	TemplateID         *int64     `gorm:"index" json:"template_id,omitempty"` // which prompt template assembled this request (NULL = none)
	ImageCount         *int64     `gorm:"default:null" json:"image_count,omitempty"`  // image requests: number of images (NULL = non-image)
	ImageSize          *string    `gorm:"size:16" json:"image_size,omitempty"`        // image requests: e.g. "1024x1024"
	ImageQuality       *string    `gorm:"size:16" json:"image_quality,omitempty"`     // image requests: e.g. "hd"
	// Context quality analysis (docs/plans/2026-08-18-context-analysis-design.md §4.1).
	// NULL = unanalyzed (cache-hit or analysis failure), never 0.
	SystemTokens         *int           `gorm:"default:null" json:"system_tokens,omitempty"`
	HistoryTokens        *int           `gorm:"default:null" json:"history_tokens,omitempty"`
	QuestionTokens       *int           `gorm:"default:null" json:"question_tokens,omitempty"`
	ToolTokens           *int           `gorm:"default:null" json:"tool_tokens,omitempty"`
	ToolOutputTokens     *int           `gorm:"default:null" json:"tool_output_tokens,omitempty"`
	ContextWindow        *int           `gorm:"default:null" json:"context_window,omitempty"`
	ContextUtilizationBp *int           `gorm:"default:null" json:"context_utilization_bp,omitempty"`
	AnalysisFlags        *int           `gorm:"default:null" json:"analysis_flags,omitempty"`
	ContextSnapshot      datatypes.JSON `gorm:"default:null" json:"context_snapshot,omitempty"`
	CreatedAt            time.Time      `gorm:"not null;index" json:"created_at"`
}

func (UsageLog) TableName() string { return "usage_logs" }

func ValidCurrency(c string) string {
	if c == "USD" {
		return "USD"
	}
	return "CNY"
}
