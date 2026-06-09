package model

import (
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type User struct {
	ID           int64      `gorm:"primaryKey" json:"id"`
	Username     string     `gorm:"size:64;not null" json:"username"`
	PasswordHash string     `gorm:"size:128;not null" json:"-"`
	DisplayName  string     `gorm:"size:128;not null" json:"display_name"`
	RoleID       int64      `gorm:"not null;index" json:"role_id"`
	Role         Role       `gorm:"foreignKey:RoleID" json:"role,omitempty"`
	Status       int16      `gorm:"not null;default:1" json:"status"`
	LastLoginAt  *time.Time `json:"last_login_at"`
	CreatedAt    time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt    time.Time      `gorm:"not null" json:"updated_at"`
	OrgID               *int64         `gorm:"index" json:"org_id"`
	Email               *string        `gorm:"size:255" json:"email,omitempty"`
	ForcePasswordChange bool           `gorm:"not null;default:false" json:"force_password_change,omitempty"`
	Preferences         datatypes.JSON `json:"preferences,omitempty"`
	SSOProviderID *int64         `gorm:"index" json:"sso_provider_id,omitempty"`
	SSOID         string         `gorm:"size:256" json:"-"`
	DeletedAt     gorm.DeletedAt `json:"-" gorm:"index"`
	TeamID        *int64         `json:"team_id" gorm:"-"`
	TeamName      *string        `json:"team_name" gorm:"-"`
	TeamRole      *string        `json:"team_role" gorm:"-"`
}

func (User) TableName() string { return "users" }
