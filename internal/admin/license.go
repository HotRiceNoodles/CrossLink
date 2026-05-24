package admin

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/license"
	"gorm.io/gorm"
)

type LicenseHandler struct {
	db  *gorm.DB
	cfg *config.Config
}

func NewLicenseHandler(db *gorm.DB, cfg *config.Config) *LicenseHandler {
	return &LicenseHandler{db: db, cfg: cfg}
}

func (h *LicenseHandler) Status(c *gin.Context) {
	gate := license.G()
	tier := gate.CurrentTier()

	edition := "Community"
	switch tier {
	case license.TierPro:
		edition = "Pro"
	case license.TierEnterprise:
		edition = "Enterprise"
	}

	data := gin.H{
		"tier":        tier,
		"edition":     edition,
		"is_valid":    gate.IsValid(),
		"fingerprint": gate.Fingerprint(),
		"crypto_mode": h.cfg.Crypto.Mode,
		"license_management": h.cfg.License.HeartbeatEnabled,
	}

	if tk := gate.TokenSnapshot(); tk != nil {
		data["license_id"] = tk.LicenseID
		if tk.ExpiresAt > 0 {
			data["expires_at"] = time.Unix(tk.ExpiresAt, 0).Format(time.RFC3339)
		}
		if tk.MaxNodes > 0 {
			data["max_nodes"] = tk.MaxNodes
		}
	}

	c.JSON(http.StatusOK, gin.H{"data": data})
}

// Import and Activate handlers are removed in Community edition.
// Commercial overlay provides these via ExtraRoutes.
var _ = slog.Default
