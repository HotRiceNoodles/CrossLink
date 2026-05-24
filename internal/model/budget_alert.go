package model

import (
	"time"

	"gorm.io/gorm"
)

type BudgetAlert struct {
	ID              int64          `gorm:"primaryKey" json:"id"`
	TeamID          *int64         `gorm:"index" json:"team_id"`
	KeyID           *int64         `gorm:"index" json:"key_id"`
	ThresholdPct    int16          `gorm:"not null" json:"threshold_pct"`
	WebhookURL      string         `gorm:"size:512;not null" json:"webhook_url"`
	LastTriggeredAt *time.Time     `json:"last_triggered_at"`
	CreatedAt       time.Time      `gorm:"not null;default:now()" json:"created_at"`
	DeletedAt       gorm.DeletedAt `json:"-" gorm:"index"`
}

func (BudgetAlert) TableName() string { return "budget_alerts" }

type BudgetSnapshot struct {
	ID         int64     `gorm:"primaryKey" json:"id"`
	TargetType string    `gorm:"size:16;not null;uniqueIndex:idx_snapshots_unique" json:"target_type"`
	TargetID   int64     `gorm:"not null;uniqueIndex:idx_snapshots_unique" json:"target_id"`
	PeriodKey  string    `gorm:"size:16;not null;uniqueIndex:idx_snapshots_unique" json:"period_key"`
	Spent      float64   `gorm:"type:decimal(16,8);not null;default:0" json:"spent"`
	Budget     float64   `gorm:"type:decimal(12,4);not null;default:0" json:"budget"`
	Currency   string    `gorm:"size:3;not null;default:'CNY'" json:"currency"`
	CreatedAt  time.Time `gorm:"not null;default:now()" json:"created_at"`
}

func (BudgetSnapshot) TableName() string { return "budget_snapshots" }
