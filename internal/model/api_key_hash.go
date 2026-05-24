package model

import "time"

type APIKeyHash struct {
	ID         int64      `gorm:"primaryKey" json:"id"`
	APIKeyID   int64      `gorm:"not null;index" json:"api_key_id"`
	KeyHash    string     `gorm:"size:64;uniqueIndex;not null" json:"-"`
	KeyPrefix  string     `gorm:"size:8;not null" json:"key_prefix"`
	HashAlgo   string     `gorm:"size:10;not null;default:'sha256'" json:"hash_algo"`
	IsPrimary  bool       `gorm:"not null;default:true" json:"is_primary"`
	GraceUntil *time.Time `json:"grace_until"`
	CreatedAt  time.Time  `gorm:"not null;default:now()" json:"created_at"`
}

func (APIKeyHash) TableName() string { return "api_key_hashes" }
