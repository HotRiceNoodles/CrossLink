package model

import "time"

// SsrfAllowlistConfig is the SINGLETON row (ID always 1) holding the
// page-configured SSRF internal-allowlist override.
//
// Presence semantics are load-bearing and MUST NOT be collapsed:
//   - NO row (never configured) → no override; the effective policy falls
//     back to env CL_GATEWAY_INTERNAL_ALLOW_CIDRS → YAML
//     gateway.internal_allow_cidrs → strict default.
//   - row with Removed=false → active override; CIDRsJSON == "[]" is an
//     EXPLICIT strict policy, distinct from absence.
//   - row with Removed=true → TOMBSTONE: a reset marker. The row is kept so
//     Version stays monotonically increasing forever — a physical delete
//     would reset versions to 1 and admit ABA overwrites from stale pages
//     holding an old expected_version, and would break cross-instance
//     version-based broadcast dedup across generations.
//
// Version is the optimistic-lock counter: writers must present the version
// they read; the row update is a compare-and-swap on it. It NEVER decreases.
type SsrfAllowlistConfig struct {
	ID            int64     `gorm:"primaryKey" json:"id"`
	CIDRsJSON     string    `gorm:"column:cidrs;size:8192;not null" json:"cidrs"`
	Version       int64     `gorm:"not null" json:"version"`
	Removed       bool      `gorm:"column:removed;not null;default:false" json:"removed"`
	UpdatedBy     int64     `gorm:"not null;default:0" json:"updated_by"`
	UpdatedByName string    `gorm:"column:updated_by_name;size:64;not null;default:''" json:"updated_by_name"`
	UpdatedAt     time.Time `gorm:"not null" json:"updated_at"`
}

func (SsrfAllowlistConfig) TableName() string { return "ssrf_allowlist_configs" }
