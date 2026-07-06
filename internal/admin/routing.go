package admin

import (
	"net/http"
	"strconv"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/repository"
	"gorm.io/gorm"
)

// providerConfigWeight is one provider's configured weight for a model.
type providerConfigWeight struct {
	ProviderID  int64
	Weight      int
	DisplayName string
}

// providerActual is the aggregated actual traffic for one provider on a model.
type providerActual struct {
	Requests     int64
	Errors       int64
	AvgLatencyMs float64
	Tokens       int64
	Cost         float64
}

// routingDistRow is one row of the routing distribution response.
type routingDistRow struct {
	ProviderID      int64   `json:"provider_id"`
	ProviderName    string  `json:"provider_name"`
	ConfigWeight    int     `json:"config_weight"`
	ConfigWeightPct float64 `json:"config_weight_pct"`
	Requests        int64   `json:"requests"`
	ActualPct       float64 `json:"actual_pct"`
	Deviation       float64 `json:"deviation"` // actual_pct - config_weight_pct
	ErrorRate       float64 `json:"error_rate"`
	AvgLatencyMs    float64 `json:"avg_latency_ms"`
	Tokens          int64   `json:"tokens"`
	Cost            float64 `json:"cost"`
}

// computeRoutingDistribution merges configured weights with actual traffic into
// per-provider distribution rows. Pure function — no DB. Zero-division safe.
//
// Includes:
//   - providers configured with a weight but receiving zero traffic (requests=0)
//   - providers receiving traffic but with no weight config (weight=0, name="")
//
// Sort: actual requests desc, then config weight desc (stable).
func computeRoutingDistribution(cfg []providerConfigWeight, actual map[int64]providerActual) []routingDistRow {
	totalWeight := 0
	cfgByID := make(map[int64]providerConfigWeight, len(cfg))
	for _, c := range cfg {
		cfgByID[c.ProviderID] = c
		totalWeight += c.Weight
	}

	totalReq := int64(0)
	for _, a := range actual {
		totalReq += a.Requests
	}

	seen := make(map[int64]bool, len(cfg)+len(actual))
	rows := make([]routingDistRow, 0, len(cfg)+len(actual))

	emit := func(pid int64) {
		c := cfgByID[pid] // zero value if unconfigured
		a := actual[pid]  // zero value if no traffic
		row := routingDistRow{
			ProviderID:   pid,
			ProviderName: c.DisplayName,
			ConfigWeight: c.Weight,
			Requests:     a.Requests,
			AvgLatencyMs: a.AvgLatencyMs,
			Tokens:       a.Tokens,
			Cost:         a.Cost,
		}
		if totalWeight > 0 {
			row.ConfigWeightPct = float64(c.Weight) / float64(totalWeight)
		}
		if totalReq > 0 {
			row.ActualPct = float64(a.Requests) / float64(totalReq)
		}
		if a.Requests > 0 {
			row.ErrorRate = float64(a.Errors) / float64(a.Requests)
		}
		row.Deviation = row.ActualPct - row.ConfigWeightPct
		rows = append(rows, row)
		seen[pid] = true
	}

	// Emit configured providers first (preserves weight-desc order from caller),
	// then any orphan providers seen in actual traffic but not configured.
	for _, c := range cfg {
		if !seen[c.ProviderID] {
			emit(c.ProviderID)
		}
	}
	for pid := range actual {
		if !seen[pid] {
			emit(pid)
		}
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Requests != rows[j].Requests {
			return rows[i].Requests > rows[j].Requests
		}
		return rows[i].ConfigWeight > rows[j].ConfigWeight
	})
	return rows
}

// RoutingHandler exposes routing-distribution analytics for the admin dashboard.
type RoutingHandler struct {
	db                *gorm.DB
	providerModelRepo *repository.ProviderModelRepo
}

func NewRoutingHandler(db *gorm.DB, pmr *repository.ProviderModelRepo) *RoutingHandler {
	return &RoutingHandler{db: db, providerModelRepo: pmr}
}

type usageActualRow struct {
	ProviderID   int64
	Requests     int64
	Errors       int64
	AvgLatencyMs float64
	Tokens       int64
	Cost         float64
}

// Stats returns the actual-vs-configured traffic distribution for one model.
//
// GET /admin/routing/stats?model=glm-5.2&days=7
//
// Reads configured weights from provider_models and actual traffic from
// usage_logs (GROUP BY provider_id), then computes per-provider distribution
// and deviation. Real-time (no DataLens aggregation delay). Org-scoped.
func (h *RoutingHandler) Stats(c *gin.Context) {
	modelName := c.Query("model")
	if modelName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model parameter required"})
		return
	}
	days := 7
	if d, err := strconv.Atoi(c.DefaultQuery("days", "7")); err == nil && d > 0 && d <= 90 {
		days = d
	}
	orgID := GetOrgID(c)
	since := time.Now().AddDate(0, 0, -days)
	ctx := c.Request.Context()

	// 1. Configured weights for this model under the caller's org scope.
	pmList, err := h.providerModelRepo.FindByModelName(ctx, modelName, orgID)
	if err != nil {
		internalErr(c, err, "query provider models failed")
		return
	}
	cfg := make([]providerConfigWeight, 0, len(pmList))
	for _, pm := range pmList {
		name := pm.Provider.DisplayName
		if name == "" {
			name = pm.Provider.Name
		}
		cfg = append(cfg, providerConfigWeight{
			ProviderID:  pm.ProviderID,
			Weight:      pm.Weight,
			DisplayName: name,
		})
	}

	// 2. Actual traffic from usage_logs grouped by provider_id. The table is
	// indexed on model_requested, provider_id, created_at — single-model + time
	// window is a bounded scan, acceptable for an admin analytics endpoint.
	q := h.db.WithContext(ctx).
		Table("usage_logs").
		Select(`provider_id,
			COUNT(*) AS requests,
			SUM(CASE WHEN status_code >= 500 THEN 1 ELSE 0 END) AS errors,
			AVG(latency_ms)::float8 AS avg_latency_ms,
			SUM(input_tokens + output_tokens) AS tokens,
			SUM(cost) AS cost`).
		Where("model_requested = ? AND created_at >= ?", modelName, since).
		Where("provider_id IS NOT NULL").
		Group("provider_id")
	if orgID != 0 {
		q = q.Where("org_id = ?", orgID)
	}
	var actuals []usageActualRow
	if err := q.Scan(&actuals).Error; err != nil {
		internalErr(c, err, "query usage stats failed")
		return
	}

	actual := make(map[int64]providerActual, len(actuals))
	for _, a := range actuals {
		actual[a.ProviderID] = providerActual{
			Requests:     a.Requests,
			Errors:       a.Errors,
			AvgLatencyMs: a.AvgLatencyMs,
			Tokens:       a.Tokens,
			Cost:         a.Cost,
		}
	}

	rows := computeRoutingDistribution(cfg, actual)

	totalReq := int64(0)
	for _, a := range actual {
		totalReq += a.Requests
	}
	c.JSON(http.StatusOK, gin.H{
		"model":          modelName,
		"days":           days,
		"total_requests": totalReq,
		"providers":      rows,
	})
}
