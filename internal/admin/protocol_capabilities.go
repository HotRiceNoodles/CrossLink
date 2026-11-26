package admin

import (
	"net/http"
	"sort"

	"github.com/crosslink/internal/admission"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
	"github.com/gin-gonic/gin"
)

// GatewayCapabilitiesHandler serves the read-only protocol-capability view
// (Phase-2 frontend card): which beta tokens, dialect extension params and
// anthropic effort-supporting api_versions the RUNNING gateway admits. The
// code-level tables are the source of truth — this endpoint only mirrors
// them, so the card never drifts from a separate config store.
func GatewayCapabilitiesHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		params := make([]string, 0, len(router.DialectExtensionFields))
		for k := range router.DialectExtensionFields {
			params = append(params, k)
		}
		sort.Strings(params)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"beta_tokens":               admission.AllowedBetaTokens(),
			"dialect_params":            params,
			"anthropic_effort_versions": provider.AnthropicEffortVersions(),
		}})
	}
}
