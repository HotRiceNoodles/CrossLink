package license

import (
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"gorm.io/gorm"
)

// CommunityInit creates a Community-mode Gate without license server communication.
// The cp parameter is accepted for signature parity with Init() but is not used.
func CommunityInit(cfg config.LicenseConfig, db *gorm.DB, cp crypto.CryptoProvider) *Gate {
	g := CommunityGate()
	global = g
	return g
}
