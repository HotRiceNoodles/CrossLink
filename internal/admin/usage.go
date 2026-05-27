package admin

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

type UsageHandler struct {
	db *gorm.DB
}

func NewUsageHandler(db *gorm.DB, _ string) *UsageHandler {
	return &UsageHandler{db: db}
}

func applyUsageFilters(query *gorm.DB, c *gin.Context) *gorm.DB {
	if m := c.Query("model"); m != "" {
		query = query.Where("model_requested = ?", m)
	}
	if providerID := c.Query("provider_id"); providerID != "" {
		query = query.Where("provider_id = ?", providerID)
	}
	if teamID := c.Query("team_id"); teamID != "" {
		query = query.Where("team_id = ?", teamID)
	}
	if apiKeyID := c.Query("api_key_id"); apiKeyID != "" {
		query = query.Where("api_key_id = ?", apiKeyID)
	}
	if start := c.Query("start_date"); start != "" {
		if t, err := time.ParseInLocation("2006-01-02", start, time.Local); err == nil {
			query = query.Where("created_at >= ?", t)
		}
	}
	if end := c.Query("end_date"); end != "" {
		if t, err := time.ParseInLocation("2006-01-02", end, time.Local); err == nil {
			query = query.Where("created_at < ?", t.AddDate(0, 0, 1))
		}
	}
	if hasFallback := c.Query("has_fallback"); hasFallback == "true" {
		query = query.Where("fallback_count > 0")
	}
	if status := c.Query("status"); status != "" {
		switch status {
		case "2xx":
			query = query.Where("status_code >= 200 AND status_code < 300")
		case "4xx":
			query = query.Where("status_code >= 400 AND status_code < 500")
		case "5xx":
			query = query.Where("status_code >= 500")
		case "429":
			query = query.Where("status_code = 429")
		}
	}
	return query
}

// applyTeamScope restricts query to the caller's team for non-admin users.
// Non-admin users without a team see nothing.
func applyTeamScope(query *gorm.DB, c *gin.Context) *gorm.DB {
	if !IsAdmin(c) {
		teamID := GetTeamID(c)
		if teamID <= 0 {
			query = query.Where("1 = 0")
		} else {
			query = query.Where("team_id = ?", teamID)
		}
	}
	return query
}

func (h *UsageHandler) List(c *gin.Context) {
	query := applyTeamScope(applyUsageFilters(h.db.WithContext(c.Request.Context()).Order("created_at DESC"), c), c)

	var total int64
	applyTeamScope(applyUsageFilters(h.db.WithContext(c.Request.Context()).Model(&model.UsageLog{}), c), c).Count(&total)

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize

	var logs []model.UsageLog
	if err := query.Offset(offset).Limit(pageSize).Find(&logs).Error; err != nil {
		internalErr(c, err, "list usage failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"data": logs,
		"pagination": gin.H{
			"total":     total,
			"page":      page,
			"page_size": pageSize,
		},
	})
}

type usageStats struct {
	TotalRequests int64   `json:"total_requests"`
	TotalTokens   int64   `json:"total_tokens"`
	TotalCost     float64 `json:"total_cost"`
	AvgLatencyMs  float64 `json:"avg_latency_ms"`
	Currency      string  `json:"currency"`
}

func (h *UsageHandler) Stats(c *gin.Context) {
	base := applyTeamScope(applyUsageFilters(h.db.WithContext(c.Request.Context()).Model(&model.UsageLog{}), c), c)

	// Determine primary currency (same pattern as DailyTrend/TeamStats)
	primaryCurrency := "CNY"
	var topCur struct{ Currency string }
	base.Select("currency").
		Group("currency").
		Order("SUM(cost) DESC").
		Limit(1).
		Scan(&topCur)
	if topCur.Currency != "" {
		primaryCurrency = topCur.Currency
	}

	// Currency-agnostic metrics
	var stats usageStats
	base.Select(
		"COUNT(*) as total_requests, COALESCE(SUM(input_tokens + output_tokens), 0) as total_tokens, COALESCE(AVG(latency_ms), 0) as avg_latency_ms",
	).Scan(&stats)

	// Cost: filtered to primary currency only
	var costResult struct{ Total float64 }
	base.Where("currency = ?", primaryCurrency).
		Select("COALESCE(SUM(cost), 0) as total").
		Scan(&costResult)
	stats.TotalCost = costResult.Total
	stats.Currency = primaryCurrency

	// Per-currency breakdown (backward compat)
	var currencySums []CurrencyCostSum
	base.Select("currency, COALESCE(SUM(cost), 0) as total").
		Group("currency").
		Scan(&currencySums)

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"total_requests":   stats.TotalRequests,
			"total_tokens":     stats.TotalTokens,
			"total_cost":       stats.TotalCost,
			"avg_latency_ms":   stats.AvgLatencyMs,
			"currency":         stats.Currency,
			"cost_by_currency": currencySums,
		},
	})
}

type CurrencyCostSum struct {
	Currency string  `json:"currency"`
	Total    float64 `json:"total"`
}

type DailyStat struct {
	Date   string  `json:"date"`
	Count  int64   `json:"count"`
	Tokens int64   `json:"tokens"`
	Cost   float64 `json:"cost"`
}

func (h *UsageHandler) DailyTrend(c *gin.Context) {
	days := 7
	if d, err := strconv.Atoi(c.Query("days")); err == nil && d > 0 {
		days = d
	}
	var results []DailyStat

	// Filter to primary currency for consistent daily cost comparison
	primaryCurrency := "CNY"
	var topCur struct{ Currency string }
	currencyQuery := applyTeamScope(applyUsageFilters(h.db.WithContext(c.Request.Context()).
		Model(&model.UsageLog{}).
		Select("currency").
		Where("created_at >= ?", time.Now().AddDate(0, 0, -days)), c), c).
		Group("currency").
		Order("SUM(cost) DESC").
		Limit(1)
	currencyQuery.Scan(&topCur)
	if topCur.Currency != "" {
		primaryCurrency = topCur.Currency
	}

	dataQuery := applyTeamScope(applyUsageFilters(h.db.WithContext(c.Request.Context()).
		Model(&model.UsageLog{}), c), c).
		Select("DATE(created_at) as date, COUNT(*) as count, COALESCE(SUM(input_tokens + output_tokens), 0) as tokens, COALESCE(SUM(cost), 0) as cost").
		Where("created_at >= ? AND currency = ?", time.Now().AddDate(0, 0, -days).Truncate(24*time.Hour), primaryCurrency).
		Group("DATE(created_at)").
		Order("date ASC")
	rows, err := dataQuery.Rows()
	if err != nil {
		internalErr(c, err, "daily trend query failed")
		return
	}
	defer rows.Close()

	for rows.Next() {
		var s DailyStat
		if err := rows.Scan(&s.Date, &s.Count, &s.Tokens, &s.Cost); err != nil {
			continue
		}
		results = append(results, s)
	}

	if results == nil {
		results = []DailyStat{}
	}
	c.JSON(http.StatusOK, gin.H{"data": results, "currency": primaryCurrency})
}

type ModelDist struct {
	Model  string  `json:"model"`
	Count  int64   `json:"count"`
	Tokens int64   `json:"tokens"`
	Cost   float64 `json:"cost"`
}

func (h *UsageHandler) ModelDistribution(c *gin.Context) {
	days := 7
	if d, err := strconv.Atoi(c.Query("days")); err == nil && d > 0 {
		days = d
	}
	var results []ModelDist

	applyTeamScope(applyUsageFilters(h.db.WithContext(c.Request.Context()).
		Model(&model.UsageLog{}), c), c).
		Select("model_requested as model, COUNT(*) as count, COALESCE(SUM(input_tokens + output_tokens), 0) as tokens, COALESCE(SUM(cost), 0) as cost").
		Where("created_at >= ?", time.Now().AddDate(0, 0, -days)).
		Group("model_requested").
		Order("count DESC").
		Limit(10).
		Scan(&results)

	if results == nil {
		results = []ModelDist{}
	}
	c.JSON(http.StatusOK, gin.H{"data": results})
}

type TeamStat struct {
	TeamID        int     `json:"team_id"`
	TeamName      string  `json:"team_name"`
	TotalRequests int64   `json:"total_requests"`
	TotalTokens   int64   `json:"total_tokens"`
	TotalCost     float64 `json:"total_cost"`
}

func (h *UsageHandler) TeamStats(c *gin.Context) {
	days := 0
	if d, err := strconv.Atoi(c.Query("days")); err == nil && d > 0 {
		days = d
	}

	// Determine primary currency (same pattern as DailyTrend)
	primaryCurrency := "CNY"
	var topCur struct{ Currency string }
	currencyQuery := applyTeamScope(applyUsageFilters(h.db.WithContext(c.Request.Context()).
		Model(&model.UsageLog{}).
		Select("currency"), c), c)
	if days > 0 {
		currencyQuery = currencyQuery.Where("created_at >= ?", time.Now().AddDate(0, 0, -days))
	}
	currencyQuery.Group("currency").Order("SUM(cost) DESC").Limit(1).Scan(&topCur)
	if topCur.Currency != "" {
		primaryCurrency = topCur.Currency
	}

	var results []TeamStat
	query := applyTeamScope(applyUsageFilters(h.db.WithContext(c.Request.Context()).
		Model(&model.UsageLog{}), c), c).
		Select("usage_logs.team_id, COALESCE(teams.display_name, 'Unknown') as team_name, COUNT(*) as total_requests, COALESCE(SUM(input_tokens + output_tokens), 0) as total_tokens, COALESCE(SUM(usage_logs.cost), 0) as total_cost").
		Joins("LEFT JOIN teams ON teams.id = usage_logs.team_id").
		Where("usage_logs.currency = ?", primaryCurrency)
	if days > 0 {
		query = query.Where("created_at >= ?", time.Now().AddDate(0, 0, -days))
	}
	query.Group("usage_logs.team_id, teams.display_name").
		Order("total_cost DESC").
		Limit(10).
		Scan(&results)

	if results == nil {
		results = []TeamStat{}
	}
	c.JSON(http.StatusOK, gin.H{"data": results, "currency": primaryCurrency})
}
