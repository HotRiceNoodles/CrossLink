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
	AllowedModels datatypes.JSON `json:"allowed_models"`
	AllowedRoutes datatypes.JSON `json:"allowed_routes"`
	AllowedIPs    datatypes.JSON `gorm:"type:json" json:"allowed_ips"` // ["1.2.3.4","10.0.0.0/8"]; null/empty = no binding
	TPMLimit      int            `gorm:"not null;default:0" json:"tpm_limit"`
	RPMLimit      int            `gorm:"not null;default:0" json:"rpm_limit"`
	MaxBudget     float64        `gorm:"type:decimal(12,4);not null;default:0" json:"max_budget"`
	BudgetPeriod  string         `gorm:"size:16;not null;default:'monthly'" json:"budget_period"`
	PriceMultiplier float64      `gorm:"type:decimal(6,4);not null;default:1.0" json:"price_multiplier"` // 1.0 = no markup; 1.3 = charge customer 30% more
	MaxCalls      int            `gorm:"not null;default:0" json:"max_calls"`
	CallPeriod    string         `gorm:"size:16;not null;default:'daily'" json:"call_period"`
	ExpiresAt     *time.Time     `json:"expires_at"`
	CreatedAt     time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt     time.Time      `gorm:"not null" json:"updated_at"`
	LastUsedAt    *time.Time     `json:"last_used_at"`
	CreatedBy     string         `gorm:"size:64;not null;default:'admin'" json:"created_by"`
	CreatedByID   *int64         `gorm:"index" json:"created_by_id"`
	TeamID        *int64         `gorm:"index" json:"team_id"`
	OrgID         *int64         `gorm:"index" json:"org_id"`
	DeletedAt     gorm.DeletedAt `json:"-" gorm:"index"`
	Email         *string        `gorm:"size:255" json:"email,omitempty"`
}

func (APIKey) TableName() string { return "api_keys" }
