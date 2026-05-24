package model

import "time"

type Insight struct {
	ID          int64     `gorm:"primaryKey" json:"id"`
	Period      string    `gorm:"size:16;not null;default:'monthly'" json:"period"`
	PeriodKey   string    `gorm:"size:16;not null" json:"period_key"`
	Scope       string    `gorm:"size:16;not null;default:'global'" json:"scope"`
	ScopeID     int64     `gorm:"not null;default:0" json:"scope_id"`
	InsightType string    `gorm:"size:32;not null;default:'summary'" json:"insight_type"`
	Title       string    `gorm:"size:256;not null;default:''" json:"title"`
	Content     string    `gorm:"type:text;not null" json:"content"`
	ModelUsed   string    `gorm:"size:128;not null;default:''" json:"model_used"`
	TokensUsed  int       `gorm:"not null;default:0" json:"tokens_used"`
	CreatedAt   time.Time `gorm:"not null;default:now()" json:"created_at"`
}

func (Insight) TableName() string { return "insights" }
