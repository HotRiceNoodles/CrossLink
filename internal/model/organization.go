package model

import (
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Organization struct {
	ID          int64          `gorm:"primaryKey" json:"id"`
	Name        string         `gorm:"size:64;not null" json:"name"`
	DisplayName string         `gorm:"size:128;not null" json:"display_name"`
	Description string         `gorm:"type:text" json:"description"`
	Status      int16          `gorm:"not null;default:1" json:"status"`
	BudgetLimit float64        `gorm:"type:decimal(12,2);not null;default:0" json:"budget_limit"`
	BudgetPeriod string        `gorm:"size:16;not null;default:'monthly'" json:"budget_period"`
	RPMLimit    int            `gorm:"not null;default:0" json:"rpm_limit"`
	TPMLimit    int            `gorm:"not null;default:0" json:"tpm_limit"`
	Settings    datatypes.JSON `gorm:"type:jsonb" json:"settings"`
	CreatedByID *int64         `gorm:"index" json:"created_by_id"`
	CreatedAt   time.Time      `gorm:"not null;default:now()" json:"created_at"`
	UpdatedAt   time.Time      `gorm:"not null;default:now()" json:"updated_at"`
	DeletedAt   gorm.DeletedAt `json:"-" gorm:"index"`
}

func (Organization) TableName() string { return "organizations" }

type OrgMember struct {
	ID        int64          `gorm:"primaryKey" json:"id"`
	OrgID     int64          `gorm:"not null;index" json:"org_id"`
	UserID    int64          `gorm:"not null;index" json:"user_id"`
	Role      string         `gorm:"size:16;not null;default:'member'" json:"role"`
	JoinedAt  time.Time      `gorm:"not null;default:now()" json:"joined_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

func (OrgMember) TableName() string { return "organization_members" }
