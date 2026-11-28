package model

import (
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type ProviderModel struct {
	ID              int64          `gorm:"primaryKey" json:"id"`
	ProviderID      int64          `gorm:"not null;index" json:"provider_id"`
	ModelName       string         `gorm:"size:128;not null" json:"model_name"`
	ProviderModel   string         `gorm:"size:128;not null" json:"provider_model"`
	Weight          int            `gorm:"not null;default:0" json:"weight"`
	Priority        int            `gorm:"not null;default:1" json:"priority"`
	Status          int16          `gorm:"not null;default:1" json:"status"`
	MaxContext      *int           `json:"max_context"`
	InputPrice      float64        `gorm:"type:decimal(10,6);not null;default:0" json:"input_price"`
	OutputPrice     float64        `gorm:"type:decimal(10,6);not null;default:0" json:"output_price"`
	RoutingStrategy string         `gorm:"size:32;not null;default:'weighted_random'" json:"routing_strategy"`
	ExtraConfig     datatypes.JSON `json:"extra_config"`
	// Single-currency design: prices are denominated in the deployment's one
	// settlement currency (system_settings.billing_currency, write-once at
	// first install) — there is no per-model currency column anymore.
	// Modality is the endpoint family this mapping serves (图片方案 P3,
	// design §5): "" (default) = unclassified — endpoint-agnostic for
	// zero-break upgrades, every handler keeps it; an explicit value is a
	// HARD gate — incompatible endpoints reject with modality_mismatch.
	// NOT NULL DEFAULT '' in the DB; values from ValidModalities.
	Modality  string         `gorm:"size:24;not null;default:''" json:"modality"`
	CreatedAt time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt time.Time      `gorm:"not null" json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`

	Provider Provider `gorm:"foreignKey:ProviderID" json:"provider,omitempty"`
}

func (ProviderModel) TableName() string { return "provider_models" }

// ValidModalities enumerates provider_models.modality (design §5.1), aligned
// with the overlay Capability.Modality vocabulary (F3). "" is not listed — it
// is the column default (unclassified), always valid.
var ValidModalities = map[string]bool{
	"text":                true,
	"image":               true,
	"video":               true,
	"audio_speech":        true,
	"audio_transcription": true,
	"embedding":           true,
}

// IsValidModality reports whether v is "" or a member of ValidModalities.
func IsValidModality(v string) bool {
	return v == "" || ValidModalities[v]
}
