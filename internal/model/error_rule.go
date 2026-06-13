package model

import "time"

// ErrorClassificationRule maps upstream error signatures to a failure classification.
// Currently classification is only "quota" (persistent failure). Used by
// service.ErrorClassifier to decide persistent (long cooldown) vs transient handling.
//
// Rules are platform-level / global (no org_id). Writes are super-admin only.
type ErrorClassificationRule struct {
	ID             int64     `gorm:"primaryKey" json:"id"`
	MatchField     string    `gorm:"size:16;not null" json:"match_field"` // status|code|type|message
	Pattern        string    `gorm:"size:128;not null" json:"pattern"`    // exact for status/code/type; substring for message
	Classification string    `gorm:"size:16;not null;default:quota" json:"classification"`
	ProviderType   *string   `gorm:"size:32" json:"provider_type"`                  // NULL = global; else providers.adapter_type
	Scope          string    `gorm:"size:16;not null;default:account" json:"scope"` // account|model
	Priority       int       `gorm:"not null;default:100" json:"priority"`
	Enabled        bool      `gorm:"not null" json:"enabled"` // no gorm default: false is meaningful, must be set explicitly (DB column has DEFAULT true for raw SQL)
	CreatedAt      time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt      time.Time `gorm:"not null" json:"updated_at"`
}

func (ErrorClassificationRule) TableName() string { return "error_classification_rules" }
