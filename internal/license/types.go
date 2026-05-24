package license

import "time"

const (
	TierCommunity  = "community"
	TierPro        = "pro"
	TierEnterprise = "enterprise"
)

type VerifiedToken struct {
	LicenseID   string
	CustomerID  string
	Tier        string
	ExpiresAt   int64
	MaxNodes    int
	GraceDays   int
	Fingerprint string
	RawToken    string
}

func (v *VerifiedToken) isValid() bool {
	if v == nil {
		return false
	}
	if v.ExpiresAt == 0 {
		return true
	}
	graceExpiry := time.Unix(v.ExpiresAt, 0).
		Add(time.Duration(v.GraceDays) * 24 * time.Hour)
	return time.Now().Before(graceExpiry)
}
