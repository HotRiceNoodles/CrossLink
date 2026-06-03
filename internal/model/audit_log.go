package model

import (
	"time"

	"gorm.io/datatypes"
)

type AuditLog struct {
	ID           int64          `gorm:"primaryKey" json:"id"`
	OrgID        *int64         `gorm:"index" json:"org_id,omitempty"`
	UserID       int64          `gorm:"not null;index" json:"user_id"`
	Username     string         `gorm:"size:64;not null" json:"username"`
	Action       string         `gorm:"size:64;not null;index" json:"action"`
	ResourceType string         `gorm:"size:32;not null;index" json:"resource_type"`
	ResourceID   string         `gorm:"size:64;not null;index" json:"resource_id"`
	ResourceName string         `gorm:"size:128" json:"resource_name"`
	Detail       datatypes.JSON `json:"detail"`
	IPAddress    string         `gorm:"size:45" json:"ip_address"`
	UserAgent    string         `gorm:"size:512" json:"user_agent"`
	Status       string         `gorm:"size:16;not null;default:'success'" json:"status"`
	CreatedAt    time.Time      `gorm:"not null;index" json:"created_at"`
}

func (AuditLog) TableName() string { return "audit_logs" }
