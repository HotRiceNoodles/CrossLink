package model

import "time"

type BudgetRecommendation struct {
	ID               int64     `gorm:"primaryKey" json:"id"`
	TargetType       string    `gorm:"size:16;not null" json:"target_type"`
	TargetID         int64     `gorm:"not null" json:"target_id"`
	Period           string    `gorm:"size:16;not null;default:'monthly'" json:"period"`
	RecommendedBudget float64  `gorm:"type:decimal(12,2);not null" json:"recommended_budget"`
	CurrentBudget    float64   `gorm:"type:decimal(12,2);not null;default:0" json:"current_budget"`
	AvgPeriodSpend   float64   `gorm:"type:decimal(12,2);not null;default:0" json:"avg_period_spend"`
	GrowthRate       float64   `gorm:"type:decimal(8,4);not null;default:0" json:"growth_rate"`
	Confidence       float64   `gorm:"type:decimal(5,2);not null;default:0" json:"confidence"`
	Reasoning        string    `gorm:"type:text;not null;default:''" json:"reasoning"`
	Currency         string    `gorm:"size:3;not null;default:'CNY'" json:"currency"`
	CreatedAt        time.Time `gorm:"not null;default:now()" json:"created_at"`
}

func (BudgetRecommendation) TableName() string { return "budget_recommendations" }

type BudgetRequest struct {
	ID               int64      `gorm:"primaryKey" json:"id"`
	TargetType       string     `gorm:"size:16;not null" json:"target_type"`
	TargetID         int64      `gorm:"not null" json:"target_id"`
	Period           string     `gorm:"size:16;not null;default:'monthly'" json:"period"`
	CurrentBudget    float64    `gorm:"type:decimal(12,2);not null;default:0" json:"current_budget"`
	RequestedBudget  float64    `gorm:"type:decimal(12,2);not null" json:"requested_budget"`
	Reason           string     `gorm:"type:text;not null;default:''" json:"reason"`
	RecommendationID *int64     `json:"recommendation_id,omitempty"`
	Status           string     `gorm:"size:16;not null;default:'pending'" json:"status"`
	CreatedBy        int64      `gorm:"not null" json:"created_by"`
	ReviewedBy       *int64     `json:"reviewed_by,omitempty"`
	ReviewComment    string     `gorm:"type:text;not null;default:''" json:"review_comment"`
	ReviewedAt       *time.Time `json:"reviewed_at,omitempty"`
	CreatedAt        time.Time  `gorm:"not null;default:now()" json:"created_at"`
	UpdatedAt        time.Time  `gorm:"not null;default:now()" json:"updated_at"`
}

func (BudgetRequest) TableName() string { return "budget_requests" }
