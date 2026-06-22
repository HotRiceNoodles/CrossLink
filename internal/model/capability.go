package model

import "time"

// Capability is a virtual model alias (e.g. "anyText") that maps to an ordered
// pool of real model names via CapabilityMember rows.
type Capability struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	OrgID     int64     `gorm:"not null;default:0;uniqueIndex:idx_capability_org_name,priority:1" json:"org_id"`
	Name      string    `gorm:"size:64;not null;uniqueIndex:idx_capability_org_name,priority:2" json:"name"`
	Modality  string    `gorm:"size:16;not null;default:'text'" json:"modality"`
	Status    int16     `gorm:"not null" json:"status"` // 1=active, 0=disabled (no GORM default tag: the DB column DEFAULT 1 from the migration covers omitted inserts, and omitting the tag lets callers persist Status=0 directly)
	CreatedAt time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"not null" json:"updated_at"`

	Members []CapabilityMember `gorm:"foreignKey:CapabilityID" json:"members,omitempty"`
}

func (Capability) TableName() string { return "capabilities" }

// CapabilityMember binds one real model_name into a Capability at a given quality.
type CapabilityMember struct {
	ID           int64     `gorm:"primaryKey" json:"id"`
	CapabilityID int64     `gorm:"not null;index;uniqueIndex:idx_cm_cap_model,priority:1" json:"capability_id"`
	ModelName    string    `gorm:"size:128;not null;uniqueIndex:idx_cm_cap_model,priority:2" json:"model_name"`
	QualityScore int       `gorm:"not null;default:0" json:"quality_score"` // higher = preferred
	Status       int16     `gorm:"not null" json:"status"` // 1=active, 0=disabled (see Capability.Status comment)
	CreatedAt    time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt    time.Time `gorm:"not null" json:"updated_at"`
}

func (CapabilityMember) TableName() string { return "capability_members" }
