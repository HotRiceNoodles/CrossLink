package model

import (
	"encoding/json"
	"time"
)

type OptimizationAction struct {
	ID             int64           `gorm:"primaryKey" json:"id"`
	OrgID          *int64          `gorm:"index" json:"org_id,omitempty"`
	ActionType     string          `gorm:"size:32;not null" json:"action_type"`
	Title          string          `gorm:"type:text;not null" json:"title"`
	Description    string          `gorm:"type:text;not null" json:"description"`
	Priority       string          `gorm:"size:16;not null;default:'medium'" json:"priority"`
	Status         string          `gorm:"size:16;not null;default:'pending'" json:"status"`
	Payload        json.RawMessage `gorm:"not null;default:'{}'" json:"payload"`
	SavingEstimate float64         `gorm:"type:decimal(12,2);default:0" json:"saving_estimate"`
	CreatedAt      time.Time       `gorm:"not null" json:"created_at"`
	AppliedAt      *time.Time      `json:"applied_at,omitempty"`
	AppliedBy      *int64          `json:"applied_by,omitempty"`
	DismissedAt    *time.Time      `json:"dismissed_at,omitempty"`
	DismissedBy    *int64          `json:"dismissed_by,omitempty"`
}

func (OptimizationAction) TableName() string { return "optimization_actions" }
