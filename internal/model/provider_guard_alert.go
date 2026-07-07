package model

import (
	"time"

	"gorm.io/datatypes"
)

// ProviderGuardAlertRule is an Enterprise-tier alert rule for per-(provider,
// model) guardrail exceedance (B1 Enterprise). When a provider exceeds its RPM
// or concurrency cap, ProviderGuardAlertService looks up matching rules and
// dispatches to their channels (webhook / IM / email) with a per-rule cooldown.
//
// Empty provider_name/model_name/limit_type act as wildcards (match any).
// org_id NULL = global (matches all orgs); non-null = org-scoped.
//
// The rule table + model live in Community (migrations + models are shared),
// but the CRUD service + admin API are commercial-overlay only (Enterprise-gated).
type ProviderGuardAlertRule struct {
	ID              int64          `gorm:"primaryKey" json:"id"`
	OrgID           *int64         `gorm:"index:idx_pgar_org_enabled" json:"org_id,omitempty"`
	ProviderName    string         `gorm:"size:64;not null;default:''" json:"provider_name"`
	ModelName       string         `gorm:"size:128;not null;default:''" json:"model_name"`
	LimitType       string         `gorm:"size:16;not null;default:''" json:"limit_type"` // 'conc' | 'rpm' | '' = any
	Channels        datatypes.JSON `gorm:"not null;default:'[]'" json:"channels"`
	CooldownSeconds int            `gorm:"not null;default:300" json:"cooldown_seconds"`
	Enabled         bool           `gorm:"not null;default:true;index:idx_pgar_org_enabled" json:"enabled"`
	LastTriggeredAt *time.Time     `json:"last_triggered_at,omitempty"`
	CreatedAt       time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt       time.Time      `gorm:"not null" json:"updated_at"`
}

func (ProviderGuardAlertRule) TableName() string { return "provider_guard_alert_rules" }
