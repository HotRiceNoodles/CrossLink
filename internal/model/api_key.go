package model

import (
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type APIKey struct {
	ID            int64          `gorm:"primaryKey" json:"id"`
	Name          string         `gorm:"size:128;not null" json:"name"`
	KeyHash       string         `gorm:"size:64;uniqueIndex;not null" json:"-"`
	KeyPrefix     string         `gorm:"size:8;not null" json:"key_prefix"`
	Status        int16          `gorm:"not null;default:1" json:"status"`
	AllowedModels datatypes.JSON `gorm:"type:jsonb" json:"allowed_models"`
	AllowedRoutes datatypes.JSON `gorm:"type:jsonb" json:"allowed_routes"`
	TPMLimit      int            `gorm:"not null;default:0" json:"tpm_limit"`
	RPMLimit      int            `gorm:"not null;default:0" json:"rpm_limit"`
	MaxBudget     float64        `gorm:"type:decimal(12,4);not null;default:0" json:"max_budget"`
	BudgetPeriod  string         `gorm:"size:16;not null;default:'monthly'" json:"budget_period"`
	ExpiresAt     *time.Time     `json:"expires_at"`
	CreatedAt     time.Time      `gorm:"not null;default:now()" json:"created_at"`
	UpdatedAt     time.Time      `gorm:"not null;default:now()" json:"updated_at"`
	LastUsedAt    *time.Time     `json:"last_used_at"`
	CreatedBy     string         `gorm:"size:64;not null;default:'admin'" json:"created_by"`
	CreatedByID   *int64         `gorm:"index" json:"created_by_id"`
	TeamID        *int64         `gorm:"index" json:"team_id"`
	DeletedAt     gorm.DeletedAt `json:"-" gorm:"index"`
}

func (APIKey) TableName() string { return "api_keys" }
