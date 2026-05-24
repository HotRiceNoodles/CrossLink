package model

import (
	"time"
)

type SystemSetting struct {
	Key       string    `gorm:"size:128;primaryKey" json:"key"`
	Value     string    `gorm:"not null" json:"value"`
	UpdatedAt time.Time `gorm:"not null;default:now()" json:"updated_at"`
}

func (SystemSetting) TableName() string { return "system_settings" }
