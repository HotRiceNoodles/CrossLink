package model

import (
	"time"

	"gorm.io/datatypes"
)

// AgentToolsetProfile is the per-(API key, tool) current state of the agent
// toolset profile (design: docs/design/agent-toolset.md). Raw schemas are
// never persisted — only the (config-truncated) description plus the sha256
// and byte size of the current definition. The unique (api_key_id, tool_name)
// key lives in the migration.
type AgentToolsetProfile struct {
	ID                int64      `gorm:"primaryKey" json:"id"`
	OrgID             int64      `gorm:"index;not null;default:0" json:"org_id"`
	TeamID            *int64     `gorm:"index" json:"team_id,omitempty"`
	APIKeyID          int64      `gorm:"index;not null" json:"api_key_id"`
	ToolName          string     `gorm:"size:256;not null" json:"tool_name"`
	Description       *string    `gorm:"type:text" json:"description,omitempty"`
	SchemaSHA256      string     `gorm:"size:64;not null;default:''" json:"schema_sha256"`
	SchemaBytes       int        `gorm:"not null;default:0" json:"schema_bytes"`
	DefinitionVersion int        `gorm:"not null;default:1" json:"definition_version"`
	Present           bool       `gorm:"not null;default:true" json:"present"` // false = gone from the server's last complete inventory (disappeared); reappearing flips it back without a new first_seen
	DeclaredCount     int64      `gorm:"not null;default:0" json:"declared_count"`
	InvokedCount      int64      `gorm:"not null;default:0" json:"invoked_count"`
	FirstSeenAt       time.Time  `gorm:"not null" json:"first_seen_at"`
	LastDeclaredAt    *time.Time `json:"last_declared_at,omitempty"`
	LastInvokedAt     *time.Time `json:"last_invoked_at,omitempty"`
	LastSeenAt        time.Time  `gorm:"not null" json:"last_seen_at"`
	LastSource        string     `gorm:"size:16;not null;default:'proxy'" json:"last_source"`
	LastModel         string     `gorm:"size:128;not null;default:''" json:"last_model"`
}

func (AgentToolsetProfile) TableName() string { return "agent_toolset_profiles" }

// AgentToolsetEvent records toolset changes (first_seen / definition_changed /
// disappeared) powering the profile history view and the M2 change alerts.
type AgentToolsetEvent struct {
	ID              int64     `gorm:"primaryKey" json:"id"`
	OrgID           int64     `gorm:"index;not null;default:0" json:"org_id"`
	APIKeyID        int64     `gorm:"index;not null" json:"api_key_id"`
	ToolName        string    `gorm:"size:256;not null" json:"tool_name"`
	EventType       string    `gorm:"size:24;not null" json:"event_type"`
	OldSchemaSHA256 string    `gorm:"size:64;not null;default:''" json:"old_schema_sha256"`
	NewSchemaSHA256 string    `gorm:"size:64;not null;default:''" json:"new_schema_sha256"`
	Source          string    `gorm:"size:16;not null;default:'proxy'" json:"source"`
	CreatedAt       time.Time `gorm:"index;not null" json:"created_at"`
}

func (AgentToolsetEvent) TableName() string { return "agent_toolset_events" }

// AgentToolsetAlertRule routes toolset change events to notification
// channels (M2). org_id NULL = global fallback rule (matched when the event's
// org has no rule of its own). event_types is a CSV of first_seen /
// definition_changed / disappeared; empty = all. Cooldown is rule-level, like
// the guardrail and provider-guard alert rules.
type AgentToolsetAlertRule struct {
	ID              int64          `gorm:"primaryKey" json:"id"`
	OrgID           *int64         `gorm:"index:idx_tsar_org_enabled" json:"org_id,omitempty"`
	EventTypes      string         `gorm:"size:64;not null;default:''" json:"event_types"`
	Channels        datatypes.JSON `gorm:"not null;default:'[]'" json:"channels"`
	CooldownMinutes int            `gorm:"not null;default:5" json:"cooldown_minutes"`
	Enabled         bool           `gorm:"not null;default:true;index:idx_tsar_org_enabled" json:"enabled"`
	LastTriggeredAt *time.Time     `json:"last_triggered_at,omitempty"`
	CreatedAt       time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt       time.Time      `gorm:"not null" json:"updated_at"`
}

func (AgentToolsetAlertRule) TableName() string { return "agent_toolset_alert_rules" }
