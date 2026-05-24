package guardrail

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Direction string

const (
	DirectionRequest  Direction = "request"
	DirectionResponse Direction = "response"
	DirectionBoth     Direction = "both"
)

type GuardrailResult struct {
	Blocked        bool   `json:"blocked"`
	RuleName       string `json:"rule_name,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Severity       string `json:"severity"`
	MaskedContent  string `json:"masked_content,omitempty"`
	ContentSnippet string `json:"content_snippet,omitempty"`
}

type GuardrailEngine interface {
	Name() string
	Check(ctx context.Context, content string, direction Direction, model string) (*GuardrailResult, error)
}

type EngineCloser interface {
	Close()
}

type GuardrailRule struct {
	ID          int64          `gorm:"primaryKey" json:"id"`
	Name        string         `gorm:"size:255;not null" json:"name"`
	Type        string         `gorm:"size:50;not null" json:"type"`
	Direction   string         `gorm:"size:10;not null" json:"direction"`
	Enabled     bool           `gorm:"default:true" json:"enabled"`
	Config      datatypes.JSON `gorm:"type:jsonb;not null" json:"config"`
	Severity    string         `gorm:"size:20;default:'medium'" json:"severity"`
	Action      string         `gorm:"size:20;default:'block'" json:"action"`
	ModelFilter *string        `gorm:"type:text" json:"model_filter"`
	CreatedAt   time.Time      `gorm:"not null;default:now()" json:"created_at"`
	UpdatedAt   time.Time      `gorm:"not null;default:now()" json:"updated_at"`
}

func (GuardrailRule) TableName() string { return "guardrail_rules" }

type CheckRequest struct {
	Content   string
	Direction Direction
	Model     string
	APIKeyID  int64
	TeamID    int64
}

type CheckResponse struct {
	Blocked       bool   `json:"blocked"`
	RuleName      string `json:"rule_name,omitempty"`
	Reason        string `json:"reason,omitempty"`
	MaskedContent string `json:"masked_content,omitempty"`
	Action        string `json:"action,omitempty"`
}

type GuardrailEvent struct {
	RuleID         int64
	RuleName       string
	EngineType     string
	Direction      string
	Severity       string
	Action         string // pass/blocked/mask/log/error
	Reason         string
	Model          string
	ContentPreview string
	APIKeyID       int64
	TeamID         int64
	Timestamp      time.Time
	AgentType      string // from CheckContext, set by agent_fingerprint engine
}

type EventHandler func(ctx context.Context, event GuardrailEvent)

type ModelGuardrailConfig struct {
	Enabled        bool     `json:"enabled"`
	RequestChecks  []string `json:"request_checks"`
	ResponseChecks []string `json:"response_checks"`
}

// EngineDeps holds infrastructure dependencies for V2 guardrail engines.
// All fields are optional — engines must check for nil before use.
type EngineDeps struct {
	Redis            redis.Cmdable // optional, nil when Redis is unavailable
	RedisClient      *redis.Client // optional, full client for Pub/Sub; nil when unavailable or Cmdable-only
	DB               *gorm.DB      // optional, nil when DB is unavailable
	IsDebugEnabled   func() bool   // optional, nil = false; reads debug.Store.IsEnabled()
	CheckLicenseTier func() string // optional, nil = "community"; returns "community"/"pro"/"enterprise"
}

// CheckContext is a mutable carrier for inter-engine state within a single Check call.
// Passed via context (CtxKeyCheck), engines read/write through the shared pointer.
type CheckContext struct {
	AgentType string            // set by agent_fingerprint engine
	RiskLevel string            // set by agent_fingerprint engine
	Metadata  map[string]string // extensible field for inter-engine data
}

// CtxKey is the type for guardrail context keys.
type CtxKey string

const (
	CtxKeyUserAgent CtxKey = "guardrail_user_agent"
	CtxKeyHeaders   CtxKey = "guardrail_headers"
	CtxKeyCheck     CtxKey = "guardrail_check" // *CheckContext pointer
)
