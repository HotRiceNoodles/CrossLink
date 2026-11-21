package model

import (
	"time"

	"gorm.io/datatypes"
)

type GuardrailAlertRule struct {
	ID              int64          `gorm:"primaryKey" json:"id"`
	OrgID           *int64         `gorm:"index" json:"org_id,omitempty"`
	RuleID          int64          `gorm:"uniqueIndex;not null" json:"rule_id"`
	TeamID          *int64         `gorm:"index" json:"team_id,omitempty"`
	Channels        datatypes.JSON `gorm:"not null;default:'[]'" json:"channels"`
	CooldownMinutes int            `gorm:"not null;default:5" json:"cooldown_minutes"`
	Enabled         bool           `gorm:"not null;default:true" json:"enabled"`
	LastTriggeredAt *time.Time     `json:"last_triggered_at,omitempty"`
	CreatedAt       time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt       time.Time      `gorm:"not null" json:"updated_at"`
}

func (GuardrailAlertRule) TableName() string { return "guardrail_alert_rules" }

type GuardrailAlertLog struct {
	ID             int64  `gorm:"primaryKey" json:"id"`
	OrgID          *int64 `gorm:"index" json:"org_id,omitempty"`
	RuleID         int64  `gorm:"not null" json:"rule_id"`
	AlertRuleID    int64  `json:"alert_rule_id"`
	RuleName       string `gorm:"size:255" json:"rule_name"`
	EngineType     string `gorm:"size:50" json:"engine_type"`
	Severity       string `gorm:"size:20" json:"severity"`
	Action         string `gorm:"size:20" json:"action"`
	Direction      string `gorm:"size:10" json:"direction"`
	Reason         string `gorm:"size:1000" json:"reason"`
	Model          string `gorm:"size:255" json:"model"`
	ContentPreview string `gorm:"size:500" json:"content_preview,omitempty"`
	APIKeyID       int64  `json:"api_key_id"`
	TeamID         int64  `json:"team_id"`
	AgentType      string `gorm:"size:32" json:"agent_type,omitempty"`
	// RuleVersion: the version of the rule copy that produced this event
	// (per-instance cached view; cross-instance propagation is TTL/broadcast
	// bounded, not atomic).
	RuleVersion int64 `gorm:"not null;default:0" json:"rule_version"`
	// Matched: the participating (rule_id, version, action) list for decision
	// events — ids/versions/actions only, never content, so it needs no
	// sanitization and stays auditable.
	Matched   datatypes.JSON `json:"matched,omitempty"`
	Channels  string         `gorm:"size:500" json:"channels"`
	Status    string         `gorm:"size:20" json:"status"`
	CreatedAt time.Time      `gorm:"not null" json:"created_at"`
	// EventID: the stable identity assigned in GuardrailService.emitEvent,
	// shared with compliance_incidents for the same event. Rows predating this
	// column are NULL and simply never participate in event-deduped statistics.
	EventID string `gorm:"size:36;index" json:"event_id,omitempty"`
	// Scores: the engine's structured decision numbers (GuardrailEvent.Scores
	// passthrough) for engines with continuous scoring. Adaptive tuning reads
	// THIS, never the Reason text. NULL for engines without scores.
	Scores datatypes.JSON `json:"scores,omitempty"`
	// Arbitration (adaptive tuning M1): a human verdict on this detection.
	// NULL = not arbitrated; engine_error rows are never arbitratable.
	Decision      string     `gorm:"size:16" json:"decision,omitempty"` // false_positive|confirmed
	DecidedBy     int64      `json:"decided_by,omitempty"`
	DecidedByName string     `gorm:"size:64" json:"decided_by_name,omitempty"`
	DecidedAt     *time.Time `json:"decided_at,omitempty"`
	DecisionNote  string     `gorm:"size:500" json:"decision_note,omitempty"`
}

func (GuardrailAlertLog) TableName() string { return "guardrail_alert_logs" }
