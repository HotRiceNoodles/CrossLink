package model

import (
	"time"

	"gorm.io/datatypes"
)

type GuardrailAlertRule struct {
	ID              int64          `gorm:"primaryKey" json:"id"`
	RuleID          int64          `gorm:"uniqueIndex;not null" json:"rule_id"`
	TeamID          *int64         `gorm:"index" json:"team_id,omitempty"`
	Channels        datatypes.JSON `gorm:"type:jsonb;not null;default:'[]'" json:"channels"`
	CooldownMinutes int            `gorm:"not null;default:5" json:"cooldown_minutes"`
	Enabled         bool           `gorm:"not null;default:true" json:"enabled"`
	LastTriggeredAt *time.Time     `json:"last_triggered_at,omitempty"`
	CreatedAt       time.Time      `gorm:"not null;default:now()" json:"created_at"`
	UpdatedAt       time.Time      `gorm:"not null;default:now()" json:"updated_at"`
}

func (GuardrailAlertRule) TableName() string { return "guardrail_alert_rules" }

type GuardrailAlertLog struct {
	ID             int64     `gorm:"primaryKey" json:"id"`
	RuleID         int64     `gorm:"not null" json:"rule_id"`
	AlertRuleID    int64     `json:"alert_rule_id"`
	RuleName       string    `gorm:"size:255" json:"rule_name"`
	EngineType     string    `gorm:"size:50" json:"engine_type"`
	Severity       string    `gorm:"size:20" json:"severity"`
	Action         string    `gorm:"size:20" json:"action"`
	Direction      string    `gorm:"size:10" json:"direction"`
	Reason         string    `gorm:"size:1000" json:"reason"`
	Model          string    `gorm:"size:255" json:"model"`
	ContentPreview string    `gorm:"size:500" json:"content_preview,omitempty"`
	APIKeyID       int64     `json:"api_key_id"`
	TeamID         int64     `json:"team_id"`
	AgentType      string    `gorm:"size:32" json:"agent_type,omitempty"`
	Channels       string    `gorm:"size:500" json:"channels"`
	Status         string    `gorm:"size:20" json:"status"`
	CreatedAt      time.Time `gorm:"not null;default:now()" json:"created_at"`
}

func (GuardrailAlertLog) TableName() string { return "guardrail_alert_logs" }
