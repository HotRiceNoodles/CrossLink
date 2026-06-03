package model

import (
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Provider struct {
	ID          int64          `gorm:"primaryKey" json:"id"`
	Name        string         `gorm:"size:64;uniqueIndex;not null" json:"name"`
	DisplayName string         `gorm:"size:128;not null" json:"display_name"`
	AdapterType string         `gorm:"size:32;not null" json:"adapter_type"`
	BaseURL     string         `gorm:"size:512;not null" json:"base_url"`
	APIKey      string         `gorm:"not null" json:"-"`
	ExtraConfig datatypes.JSON `json:"extra_config"`
	Status      int16          `gorm:"not null;default:1" json:"status"`
	OrgID       *int64         `gorm:"index" json:"org_id"`
	CreatedAt   time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt   time.Time      `gorm:"not null" json:"updated_at"`
	DeletedAt   gorm.DeletedAt `json:"-" gorm:"index"`
}

func (Provider) TableName() string { return "providers" }
