package model

import (
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type ProviderModel struct {
	ID            int64          `gorm:"primaryKey" json:"id"`
	ProviderID    int64          `gorm:"not null;index" json:"provider_id"`
	ModelName     string         `gorm:"size:128;not null" json:"model_name"`
	ProviderModel string         `gorm:"size:128;not null" json:"provider_model"`
	Weight        int            `gorm:"not null;default:0" json:"weight"`
	Priority      int            `gorm:"not null;default:1" json:"priority"`
	Status        int16          `gorm:"not null;default:1" json:"status"`
	MaxContext    *int           `json:"max_context"`
	InputPrice    float64        `gorm:"type:decimal(10,6);not null;default:0" json:"input_price"`
	OutputPrice   float64        `gorm:"type:decimal(10,6);not null;default:0" json:"output_price"`
	Currency        string         `gorm:"size:3;not null;default:'CNY'" json:"currency"`
	RoutingStrategy string         `gorm:"size:32;not null;default:'weighted_random'" json:"routing_strategy"`
	ExtraConfig     datatypes.JSON `gorm:"type:jsonb" json:"extra_config"`
	CreatedAt     time.Time      `gorm:"not null;default:now()" json:"created_at"`
	UpdatedAt     time.Time      `gorm:"not null;default:now()" json:"updated_at"`
	DeletedAt     gorm.DeletedAt `json:"-" gorm:"index"`

	Provider Provider `gorm:"foreignKey:ProviderID" json:"provider,omitempty"`
}

func (ProviderModel) TableName() string { return "provider_models" }
