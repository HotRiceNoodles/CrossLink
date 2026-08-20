package admin

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
)

// PATReadHandler serves the PAT-authenticated read-only routes
// (/admin/api/pat/*). Keys is implemented (T7); others T8-T9.
type PATReadHandler struct {
	keyLister   PATKeyLister
	usageSummer UsageSummer
	agg         usageAggregator
	budgetSpent BudgetSpent
	health      PATHealth
	version     string
}

// PATKeyLister is the consumer-side interface for listing keys.
type PATKeyLister interface {
	List(ctx context.Context, orgID int64) ([]model.APIKey, error)
}

// UsageAgg holds per-key usage totals for the current UTC day.
type UsageAgg struct {
	Requests int64
	Tokens   int64
	Cost     float64
}

// UsageSummer aggregates today's usage per API key.
type UsageSummer interface {
	TodayByKey(ctx context.Context, keyIDs []int64) (map[int64]UsageAgg, error)
}

// DailyAgg holds one day's usage totals.
type DailyAgg struct {
	Date     string
	Requests int64
	Tokens   int64
	Cost     float64
}

// usageAggregator aggregates usage_logs by day (consumer-side interface).
type usageAggregator interface {
	DailySummary(ctx context.Context, since time.Time) ([]DailyAgg, error)
}

// GormUsageAggregator is the real usageAggregator backed by usage_logs.
// SQL shape follows UsageHandler.DailyTrend: DATE(created_at) bucketing.
type GormUsageAggregator struct {
	DB *gorm.DB
}

func (a *GormUsageAggregator) DailySummary(ctx context.Context, since time.Time) ([]DailyAgg, error) {
	var rows []DailyAgg
	err := a.DB.WithContext(ctx).
		Table("usage_logs").
		Select("DATE(created_at) as date, COUNT(*) as requests, COALESCE(SUM(input_tokens + output_tokens), 0) as tokens, COALESCE(SUM(cost), 0) as cost").
		Where("created_at >= ?", since).
		Group("DATE(created_at)").
		Order("date ASC").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// BudgetSpent is the consumer-side interface for current-period spend.
type BudgetSpent interface {
	GetCurrentSpent(ctx context.Context, scope, targetID, period string) float64
}

// PATHealth is the consumer-side interface for circuit snapshots.
type PATHealth interface {
	Snapshot() []provider.ProviderHealthSnapshot
}

// PATReadDeps collects the handler's dependencies.
type PATReadDeps struct {
	KeyLister   PATKeyLister
	UsageSummer UsageSummer
	UsageAgg    usageAggregator
	BudgetSpent BudgetSpent
	Health      PATHealth
	Version     string
}

func NewPATReadHandler(deps PATReadDeps) *PATReadHandler {
	return &PATReadHandler{
		keyLister:   deps.KeyLister,
		usageSummer: deps.UsageSummer,
		agg:         deps.UsageAgg,
		budgetSpent: deps.BudgetSpent,
		health:      deps.Health,
		version:     deps.Version,
	}
}

// GormUsageSummer is the real UsageSummer backed by usage_logs.
type GormUsageSummer struct {
	DB *gorm.DB
}

func (s *GormUsageSummer) TodayByKey(ctx context.Context, keyIDs []int64) (map[int64]UsageAgg, error) {
	result := make(map[int64]UsageAgg, len(keyIDs))
	if len(keyIDs) == 0 {
		return result, nil
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	var rows []struct {
		APIKeyID int64
		Req      int64
		Tokens   int64
		Cost     float64
	}
	err := s.DB.WithContext(ctx).
		Table("usage_logs").
		Select("api_key_id, COUNT(*) as req, COALESCE(SUM(input_tokens + output_tokens), 0) as tokens, COALESCE(SUM(cost), 0) as cost").
		Where("api_key_id IN ? AND created_at >= ?", keyIDs, today).
		Group("api_key_id").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		result[r.APIKeyID] = UsageAgg{Requests: r.Req, Tokens: r.Tokens, Cost: r.Cost}
	}
	return result, nil
}

// patKeyDTO is the field whitelist for /pat/keys — never serialize model.APIKey.
type patKeyDTO struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	Status        int16      `json:"status"`
	ExpiresAt     *time.Time `json:"expires_at"`
	TodayRequests int64      `json:"today_requests"`
	TodayTokens   int64      `json:"today_tokens"`
	TodayCost     float64    `json:"today_cost"`
}

func (h *PATReadHandler) Keys(c *gin.Context) {
	keys, err := h.keyLister.List(c.Request.Context(), GetOrgID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list keys"})
		return
	}

	keyIDs := make([]int64, len(keys))
	for i, k := range keys {
		keyIDs[i] = k.ID
	}
	agg, err := h.usageSummer.TodayByKey(c.Request.Context(), keyIDs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to aggregate usage"})
		return
	}

	items := make([]patKeyDTO, 0, len(keys))
	for _, k := range keys {
		a := agg[k.ID]
		items = append(items, patKeyDTO{
			ID:            k.ID,
			Name:          k.Name,
			Status:        k.Status,
			ExpiresAt:     k.ExpiresAt,
			TodayRequests: a.Requests,
			TodayTokens:   a.Tokens,
			TodayCost:     a.Cost,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": items})
}

// patDailyDTO is the field whitelist for /pat/usage/summary — time bucket +
// aggregates only, never per-request rows or user content.
type patDailyDTO struct {
	Date     string  `json:"date"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
}

type patUsageTotalDTO struct {
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
}

func (h *PATReadHandler) Usage(c *gin.Context) {
	days := 7
	if d, err := strconv.Atoi(c.Query("days")); err == nil && d > 0 && d <= 90 {
		days = d
	}
	since := time.Now().UTC().AddDate(0, 0, -days).Truncate(24 * time.Hour)

	rows, err := h.agg.DailySummary(c.Request.Context(), since)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to aggregate usage"})
		return
	}

	dayItems := make([]patDailyDTO, 0, len(rows))
	var total patUsageTotalDTO
	for _, r := range rows {
		dayItems = append(dayItems, patDailyDTO{Date: r.Date, Requests: r.Requests, Tokens: r.Tokens, Cost: r.Cost})
		total.Requests += r.Requests
		total.Tokens += r.Tokens
		total.Cost += r.Cost
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"days": dayItems, "total": total}})
}

// Budget status thresholds.
const (
	budgetWarnRatio  = 0.8
	budgetScopeKey   = "key"
	budgetStatusExceeded  = "exceeded"
	budgetStatusWarning   = "warning"
	budgetStatusOK        = "ok"
)

// patBudgetDTO is the field whitelist for /pat/budgets/status — no key
// internals, no notification config.
type patBudgetDTO struct {
	Name       string  `json:"name"`
	Limit      float64 `json:"limit"`
	Spent      float64 `json:"spent"`
	Percentage float64 `json:"percentage"`
	Status     string  `json:"status"`
}

func (h *PATReadHandler) Budgets(c *gin.Context) {
	keys, err := h.keyLister.List(c.Request.Context(), GetOrgID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list keys"})
		return
	}

	items := make([]patBudgetDTO, 0, len(keys))
	for _, k := range keys {
		if k.MaxBudget <= 0 {
			continue // no budget configured — skip
		}
		spent := h.budgetSpent.GetCurrentSpent(c.Request.Context(), budgetScopeKey, strconv.FormatInt(k.ID, 10), k.BudgetPeriod)
		pct := spent / k.MaxBudget * 100
		status := budgetStatusOK
		switch {
		case spent >= k.MaxBudget:
			status = budgetStatusExceeded
		case spent >= k.MaxBudget*budgetWarnRatio:
			status = budgetStatusWarning
		}
		items = append(items, patBudgetDTO{
			Name:       k.Name,
			Limit:      k.MaxBudget,
			Spent:      spent,
			Percentage: pct,
			Status:     status,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": items})
}

// patHealthProviderDTO is the field whitelist for /pat/health providers —
// never provider URL, credentials, or secret config.
type patHealthProviderDTO struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Circuit  string `json:"circuit"`
}

func (h *PATReadHandler) Health(c *gin.Context) {
	snaps := h.health.Snapshot()
	providers := make([]patHealthProviderDTO, 0, len(snaps))
	for _, s := range snaps {
		providers = append(providers, patHealthProviderDTO{
			Provider: s.Provider,
			Model:    s.Model,
			Circuit:  s.State,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"version": h.version, "providers": providers}})
}
