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

// applyOrgScope restricts query to the caller's org when org context is set.
func applyOrgScope(query *gorm.DB, c *gin.Context) *gorm.DB {
	orgID := GetOrgID(c)
	if orgID != 0 {
		query = query.Where("org_id = ?", orgID)
	}
	return query
}

func (h *UsageHandler) List(c *gin.Context) {
	query := applyOrgScope(applyTeamScope(applyUsageFilters(h.db.WithContext(c.Request.Context()).Order("created_at DESC"), c), c), c)

	var total int64
	applyOrgScope(applyTeamScope(applyUsageFilters(h.db.WithContext(c.Request.Context()).Model(&model.UsageLog{}), c), c), c).Count(&total)

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
	TotalRequests   int64   `json:"total_requests"`
	TotalSessions   int64   `json:"total_sessions"`
	TotalTokens     int64   `json:"total_tokens"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	ReasoningTokens int64   `json:"reasoning_tokens"`
	CacheReadTokens int64   `json:"cache_read_tokens"`
	TotalCost       float64 `json:"total_cost"`
	CostPer1kTokens float64 `json:"cost_per_1k_tokens"`
	CostPerRequest  float64 `json:"cost_per_request"`
	AvgLatencyMs    float64 `json:"avg_latency_ms"`
	AvgFirstTokenMs float64 `json:"avg_first_token_ms"`
	ErrorRate       float64 `json:"error_rate"`
	ActiveAPIKeys   int64   `json:"active_api_keys"`
	FallbackRate    float64 `json:"fallback_rate"`
	RetryRate       float64 `json:"retry_rate"`
	GuardrailRate   float64 `json:"guardrail_block_rate"`
	Currency        string  `json:"currency"`
}

func (h *UsageHandler) Stats(c *gin.Context) {
	base := applyOrgScope(applyTeamScope(applyUsageFilters(h.db.WithContext(c.Request.Context()).Model(&model.UsageLog{}), c), c), c)

	// Default 30-day range when no start_date specified
	if c.Query("start_date") == "" {
		base = base.Where("created_at >= ?", time.Now().AddDate(0, 0, -30))
	}

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

	// Main aggregation query
	var stats usageStats
	base.Select(
		"COUNT(*) as total_requests, " +
			"COUNT(DISTINCT CASE WHEN session_id != '' THEN session_id END) as total_sessions, " +
			"COALESCE(SUM(input_tokens + output_tokens), 0) as total_tokens, " +
			"COALESCE(SUM(input_tokens), 0) as input_tokens, " +
			"COALESCE(SUM(output_tokens), 0) as output_tokens, " +
			"COALESCE(SUM(reasoning_tokens), 0) as reasoning_tokens, " +
			"COALESCE(SUM(cache_read_tokens), 0) as cache_read_tokens, " +
			"COALESCE(AVG(latency_ms), 0) as avg_latency_ms, " +
			"COALESCE(AVG(CASE WHEN first_token_ms IS NOT NULL THEN first_token_ms END), 0) as avg_first_token_ms, " +
			"COUNT(DISTINCT api_key_id) as active_api_keys",
	).Scan(&stats)

	// Error/fallback/retry/guardrail counts (same base query)
	var rates struct {
		ErrorCount     int64
		FallbackCount  int64
		RetryCount     int64
		GuardrailCount int64
	}
	base.Select(
		"COUNT(CASE WHEN status_code >= 400 THEN 1 END) as error_count, " +
			"COUNT(CASE WHEN fallback_count > 0 THEN 1 END) as fallback_count, " +
			"COUNT(CASE WHEN retry_count > 0 THEN 1 END) as retry_count, " +
			"COUNT(CASE WHEN guardrail_triggered THEN 1 END) as guardrail_count",
	).Scan(&rates)

	// Cost: filtered to primary currency only
	var costResult struct{ Total float64 }
	base.Where("currency = ?", primaryCurrency).
		Select("COALESCE(SUM(cost), 0) as total").
		Scan(&costResult)
	stats.TotalCost = costResult.Total
	stats.Currency = primaryCurrency

	// Derived metrics with zero-division guards
	if stats.TotalTokens > 0 {
		stats.CostPer1kTokens = stats.TotalCost / float64(stats.TotalTokens) * 1000
	}
	if stats.TotalRequests > 0 {
		stats.CostPerRequest = stats.TotalCost / float64(stats.TotalRequests)
		stats.ErrorRate = float64(rates.ErrorCount) / float64(stats.TotalRequests)
		stats.FallbackRate = float64(rates.FallbackCount) / float64(stats.TotalRequests)
		stats.RetryRate = float64(rates.RetryCount) / float64(stats.TotalRequests)
		stats.GuardrailRate = float64(rates.GuardrailCount) / float64(stats.TotalRequests)
	}

	// Per-currency breakdown (backward compat)
	var currencySums []CurrencyCostSum
	base.Select("currency, COALESCE(SUM(cost), 0) as total").
		Group("currency").
		Scan(&currencySums)

	data := gin.H{
		"total_requests":     stats.TotalRequests,
		"total_sessions":    stats.TotalSessions,
		"total_tokens":      stats.TotalTokens,
		"input_tokens":      stats.InputTokens,
		"output_tokens":     stats.OutputTokens,
		"reasoning_tokens":  stats.ReasoningTokens,
		"cache_read_tokens": stats.CacheReadTokens,
		"total_cost":        stats.TotalCost,
		"cost_per_1k_tokens": stats.CostPer1kTokens,
		"cost_per_request":   stats.CostPerRequest,
		"avg_latency_ms":    stats.AvgLatencyMs,
		"avg_first_token_ms": stats.AvgFirstTokenMs,
		"error_rate":        stats.ErrorRate,
		"active_api_keys":   stats.ActiveAPIKeys,
		"fallback_rate":     stats.FallbackRate,
		"retry_rate":        stats.RetryRate,
		"guardrail_block_rate": stats.GuardrailRate,
		"currency":          stats.Currency,
		"cost_by_currency":  currencySums,
	}

	// Global-view-only resource totals. These are point-in-time counts of
	// organizations / members / API keys, only meaningful when no org scope
	// is selected (org_id == 0). Count queries respect soft-delete via GORM.
	if GetOrgID(c) == 0 {
		var orgCount, memberCount, totalKeys int64
		h.db.Model(&model.Organization{}).Count(&orgCount)
		h.db.Model(&model.User{}).Count(&memberCount)
		h.db.Model(&model.APIKey{}).Count(&totalKeys)
		data["organization_count"] = orgCount
		data["member_count"] = memberCount
		data["total_api_keys"] = totalKeys
	}

	c.JSON(http.StatusOK, gin.H{"data": data})
}

type CurrencyCostSum struct {
	Currency string  `json:"currency"`
	Total    float64 `json:"total"`
}

type DailyStat struct {
	Date                string  `json:"date"`
	Count               int64   `json:"count"`
	Tokens              int64   `json:"tokens"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	ReasoningTokens     int64   `json:"reasoning_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	FallbackCountDaily  int64   `json:"fallback_count_daily"`
	RetryCountDaily     int64   `json:"retry_count_daily"`
	GuardrailCountDaily int64   `json:"guardrail_count_daily"`
	Cost                float64 `json:"cost"`
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
	currencyQuery := applyOrgScope(applyTeamScope(applyUsageFilters(h.db.WithContext(c.Request.Context()).
		Model(&model.UsageLog{}).
		Select("currency").
		Where("created_at >= ?", time.Now().AddDate(0, 0, -days)), c), c), c).
		Group("currency").
		Order("SUM(cost) DESC").
		Limit(1)
	currencyQuery.Scan(&topCur)
	if topCur.Currency != "" {
		primaryCurrency = topCur.Currency
	}

	dataQuery := applyOrgScope(applyTeamScope(applyUsageFilters(h.db.WithContext(c.Request.Context()).
		Model(&model.UsageLog{}), c), c), c).
		Select("DATE(created_at) as date, COUNT(*) as count, COALESCE(SUM(input_tokens + output_tokens), 0) as tokens, COALESCE(SUM(input_tokens), 0) as input_tokens, COALESCE(SUM(output_tokens), 0) as output_tokens, COALESCE(SUM(reasoning_tokens), 0) as reasoning_tokens, COALESCE(SUM(cache_read_tokens), 0) as cache_read_tokens, COUNT(CASE WHEN fallback_count > 0 THEN 1 END) as fallback_count_daily, COUNT(CASE WHEN retry_count > 0 THEN 1 END) as retry_count_daily, COUNT(CASE WHEN guardrail_triggered THEN 1 END) as guardrail_count_daily, COALESCE(SUM(cost), 0) as cost").
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
		if err := rows.Scan(&s.Date, &s.Count, &s.Tokens, &s.InputTokens, &s.OutputTokens, &s.ReasoningTokens, &s.CacheReadTokens, &s.FallbackCountDaily, &s.RetryCountDaily, &s.GuardrailCountDaily, &s.Cost); err != nil {
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

type TemplateStat struct {
	TemplateID    int64   `json:"template_id"`
	TemplateName  string  `json:"template_name"`
	TotalRequests int64   `json:"total_requests"`
	TotalTokens   int64   `json:"total_tokens"`
	TotalCost     float64 `json:"total_cost"`
}

// TemplateStats aggregates usage by prompt template (template_id), joining
// prompt_templates for display names. Only requests that used a template
// (template_id IS NOT NULL) are counted. Returns [] when no template usage.
func (h *UsageHandler) TemplateStats(c *gin.Context) {
	days := 7
	if d, err := strconv.Atoi(c.Query("days")); err == nil && d > 0 {
		days = d
	}
	var results []TemplateStat

	applyOrgScope(applyTeamScope(applyUsageFilters(h.db.WithContext(c.Request.Context()).
		Model(&model.UsageLog{}), c), c), c).
		Select("usage_logs.template_id as template_id, prompt_templates.name as template_name, COUNT(*) as total_requests, COALESCE(SUM(input_tokens + output_tokens), 0) as total_tokens, COALESCE(SUM(cost), 0) as total_cost").
		Joins("LEFT JOIN prompt_templates ON prompt_templates.id = usage_logs.template_id").
		Where("usage_logs.template_id IS NOT NULL AND usage_logs.created_at >= ?", time.Now().AddDate(0, 0, -days)).
		Group("usage_logs.template_id, prompt_templates.name").
		Order("total_requests DESC").
		Limit(20).
		Scan(&results)

	if results == nil {
		results = []TemplateStat{}
	}
	c.JSON(http.StatusOK, gin.H{"data": results})
}

func (h *UsageHandler) ModelDistribution(c *gin.Context) {
	days := 7
	if d, err := strconv.Atoi(c.Query("days")); err == nil && d > 0 {
		days = d
	}
	var results []ModelDist

	applyOrgScope(applyTeamScope(applyUsageFilters(h.db.WithContext(c.Request.Context()).
		Model(&model.UsageLog{}), c), c), c).
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
	currencyQuery := applyOrgScope(applyTeamScope(applyUsageFilters(h.db.WithContext(c.Request.Context()).
		Model(&model.UsageLog{}).
		Select("currency"), c), c), c)
	if days > 0 {
		currencyQuery = currencyQuery.Where("created_at >= ?", time.Now().AddDate(0, 0, -days))
	}
	currencyQuery.Group("currency").Order("SUM(cost) DESC").Limit(1).Scan(&topCur)
	if topCur.Currency != "" {
		primaryCurrency = topCur.Currency
	}

	var results []TeamStat
	query := applyOrgScope(applyTeamScope(applyUsageFilters(h.db.WithContext(c.Request.Context()).
		Model(&model.UsageLog{}), c), c), c).
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
