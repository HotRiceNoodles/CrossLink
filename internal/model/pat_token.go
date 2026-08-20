package model

import (
	"encoding/json"
	"time"

	"gorm.io/datatypes"
)

type PatToken struct {
	ID         int64          `gorm:"primaryKey" json:"id"`
	UserID     int64          `gorm:"index;not null" json:"user_id"`
	Name       string         `gorm:"size:128;not null" json:"name"`
	TokenHash  string         `gorm:"size:64;uniqueIndex;not null" json:"-"`
	Scopes     datatypes.JSON `json:"scopes"`
	Status     int16          `gorm:"not null;default:1" json:"status"`
	ExpiresAt  time.Time      `gorm:"not null" json:"expires_at"`
	CreatedAt  time.Time      `gorm:"not null" json:"created_at"`
	LastUsedAt *time.Time     `json:"last_used_at"`
	RevokedAt  *time.Time     `json:"revoked_at"`
}

func (PatToken) TableName() string { return "pat_tokens" }

// ScopeList parses the JSON scopes column into a string slice.
func (t PatToken) ScopeList() ([]string, error) {
	var scopes []string
	if len(t.Scopes) == 0 {
		return scopes, nil
	}
	err := json.Unmarshal(t.Scopes, &scopes)
	return scopes, err
}
