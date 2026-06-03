package model

import (
	"time"

	"gorm.io/datatypes"
)

type AgentFingerprint struct {
	ID             int64          `gorm:"primaryKey" json:"id"`
	OrgID          *int64         `gorm:"index" json:"org_id,omitempty"`
	Name           string         `gorm:"size:64;not null" json:"name"`
	SourceType     string         `gorm:"size:16;not null;default:'header'" json:"source_type"`
	SourceField    string         `gorm:"size:128;not null;default:''" json:"source_field"`
	Pattern        string         `gorm:"type:text;not null" json:"pattern"`
	RiskLevel      string         `gorm:"size:16;not null;default:'medium'" json:"risk_level"`
	Origin         string         `gorm:"size:16;not null" json:"origin"`
	Status         string         `gorm:"size:16;not null;default:'active'" json:"status"`
	HitCount       int64          `gorm:"not null;default:0" json:"hit_count"`
	LastHitAt      *time.Time     `json:"last_hit_at,omitempty"`
	DiscoveredFrom datatypes.JSON `json:"discovered_from,omitempty"`
	CreatedAt      time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt      time.Time      `gorm:"not null" json:"updated_at"`
}

func (AgentFingerprint) TableName() string { return "agent_fingerprints" }
