package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/dialect"
	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DataLensAggregatorService runs background aggregation from usage_logs into
// pre-aggregated hourly and daily metric tables.
type DataLensAggregatorService struct {
	db           *gorm.DB
	d            dialect.Dialect
	levels       []AggregateLevel
	mu           sync.Mutex
	interval     time.Duration
	lookback     time.Duration
	backfillDays int
	hourlyDays   int
	dailyDays    int
}

func NewDataLensAggregatorService(db *gorm.DB, d dialect.Dialect, cfg config.DataLensConfig) *DataLensAggregatorService {
	interval, _ := time.ParseDuration(cfg.Agg.Interval)
	lookback, _ := time.ParseDuration(cfg.Agg.Lookback)
	if interval == 0 {
		interval = time.Hour
	}
	if lookback == 0 {
		lookback = 3 * time.Hour
	}
	return &DataLensAggregatorService{
		db:           db,
		d:            d,
		levels:       DefaultLevels(),
		interval:     interval,
		lookback:     lookback,
		backfillDays: cfg.Agg.BackfillDays,
		hourlyDays:   cfg.Retention.HourlyDays,
		dailyDays:    cfg.Retention.DailyDays,
	}
}

// Run starts the background aggregation loop. Blocks until ctx is cancelled.
func (s *DataLensAggregatorService) Run(ctx context.Context) {
	s.backfill(ctx)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			s.runAggregationCycle(ctx)
			s.mu.Unlock()
		}
	}
}

// AggregateOnce runs one aggregation cycle on demand.
func (s *DataLensAggregatorService) AggregateOnce(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runAggregationCycle(ctx)
}

// runAggregationCycle executes hourly, daily, then cleanup. Logs errors but continues.
func (s *DataLensAggregatorService) runAggregationCycle(ctx context.Context) error {
	var firstErr error
	if err := s.aggregateHourly(ctx); err != nil {
		slog.Warn("datalens aggregator: hourly failed", "error", err)
		if firstErr == nil {
			firstErr = err
		}
	}
	if err := s.aggregateDaily(ctx); err != nil {
		slog.Warn("datalens aggregator: daily failed", "error", err)
		if firstErr == nil {
			firstErr = err
		}
	}
	if err := s.cleanupRetentionPolicy(ctx); err != nil {
		slog.Warn("datalens aggregator: cleanup failed", "error", err)
		if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// dimInfo maps a usage_logs dimension column to its SELECT alias and INSERT column name.
type dimInfo struct {
	selectExpr string // e.g. "model_requested AS model_name"
	insertCol  string // e.g. "model_name"
	groupCol   string // e.g. "model_requested" (raw column for GROUP BY)
}

var dimMapping = map[string]dimInfo{
	"model_requested": {selectExpr: "model_requested AS model_name", insertCol: "model_name", groupCol: "model_requested"},
	"team_id":         {selectExpr: "team_id", insertCol: "team_id", groupCol: "team_id"},
	"api_key_id":      {selectExpr: "api_key_id", insertCol: "api_key_id", groupCol: "api_key_id"},
	"provider_id":     {selectExpr: "provider_id", insertCol: "provider_id", groupCol: "provider_id"},
}

// aggregateHourly runs the hourly UPSERT for all levels.
func (s *DataLensAggregatorService) aggregateHourly(ctx context.Context) error {
	now := time.Now().UTC()
	start := now.Add(-s.lookback)

	var firstErr error
	for _, level := range s.levels {
		t0 := time.Now()
		sql, args := s.buildHourlySQL(level, start, now)
		result := s.db.WithContext(ctx).Exec(sql, args...)
		elapsed := time.Since(t0)
		rows := 0
		if result.RowsAffected >= 0 {
			rows = int(result.RowsAffected)
		}
		if result.Error != nil {
			slog.Warn("datalens aggregator: hourly level failed",
				"level", level.Name, "error", result.Error, "elapsed_ms", elapsed.Milliseconds())
			s.updateAggStatus(ctx, level.Name, "hourly", int(elapsed.Milliseconds()), rows, result.Error)
			if firstErr == nil {
				firstErr = fmt.Errorf("hourly %s: %w", level.Name, result.Error)
			}
			continue
		}
		slog.Info("datalens aggregator: hourly level done",
			"level", level.Name, "rows", rows, "elapsed_ms", elapsed.Milliseconds())
		s.updateAggStatus(ctx, level.Name, "hourly", int(elapsed.Milliseconds()), rows, nil)
	}
	return firstErr
}

func (s *DataLensAggregatorService) buildHourlySQL(level AggregateLevel, start, end time.Time) (string, []any) {
	hourTrunc := s.d.DateTrunc("hour", "created_at")

	// Build dimension-related SELECT / INSERT / GROUP BY fragments.
	var dimSelects, dimInserts, dimGroups []string
	for _, dim := range level.Dimensions {
		info, ok := dimMapping[dim]
		if !ok {
			continue
		}
		dimSelects = append(dimSelects, info.selectExpr)
		dimInserts = append(dimInserts, info.insertCol)
		dimGroups = append(dimGroups, info.groupCol)
	}

	// INSERT column list
	insertCols := []string{
		"org_id", "agg_level",
	}
	insertCols = append(insertCols, dimInserts...)
	insertCols = append(insertCols,
		"status_group", "hour_bucket", "currency",
		"request_count", "input_tokens", "output_tokens", "reasoning_tokens", "cache_read_tokens",
		"total_cost", "total_latency_ms", "min_latency_ms", "max_latency_ms", "latency_samples",
		"total_first_token_ms", "first_token_samples",
		"error_count", "fallback_count", "retry_count", "guardrail_blocks", "cache_hits",
		"distinct_sessions", "distinct_keys",
		"latency_bucket_50", "latency_bucket_100", "latency_bucket_200",
		"latency_bucket_500", "latency_bucket_1000", "latency_bucket_2000",
		"latency_bucket_5000", "latency_bucket_slow",
	)

	// SELECT expressions
	selectExprs := []string{
		"org_id",
		fmt.Sprintf("'%s' AS agg_level", level.Name),
	}
	selectExprs = append(selectExprs, dimSelects...)
	selectExprs = append(selectExprs,
		statusGroupCASE(),
		hourTrunc+" AS hour_bucket",
		"currency",
		"COUNT(*) AS request_count",
		"SUM(input_tokens) AS input_tokens",
		"SUM(output_tokens) AS output_tokens",
		"SUM(reasoning_tokens) AS reasoning_tokens",
		"SUM(cache_read_tokens) AS cache_read_tokens",
		"SUM(cost) AS total_cost",
		"SUM(latency_ms) AS total_latency_ms",
		"MIN(latency_ms) AS min_latency_ms",
		"MAX(latency_ms) AS max_latency_ms",
		"COUNT(*) AS latency_samples",
		"SUM(COALESCE(first_token_ms, 0)) AS total_first_token_ms",
		"COUNT(first_token_ms) AS first_token_samples",
		s.d.ConditionalSum("status_code >= 400") + " AS error_count",
		s.d.ConditionalSum("fallback_count > 0") + " AS fallback_count",
		s.d.ConditionalSum("retry_count > 0") + " AS retry_count",
		s.d.ConditionalSum("guardrail_triggered") + " AS guardrail_blocks",
		s.d.ConditionalSum("cache_hit") + " AS cache_hits",
		"COUNT(DISTINCT CASE WHEN session_id != '' THEN session_id END) AS distinct_sessions",
		"COUNT(DISTINCT api_key_id) AS distinct_keys",
		s.d.ConditionalSum("latency_ms BETWEEN 0 AND 49") + " AS latency_bucket_50",
		s.d.ConditionalSum("latency_ms BETWEEN 50 AND 99") + " AS latency_bucket_100",
		s.d.ConditionalSum("latency_ms BETWEEN 100 AND 199") + " AS latency_bucket_200",
		s.d.ConditionalSum("latency_ms BETWEEN 200 AND 499") + " AS latency_bucket_500",
		s.d.ConditionalSum("latency_ms BETWEEN 500 AND 999") + " AS latency_bucket_1000",
		s.d.ConditionalSum("latency_ms BETWEEN 1000 AND 1999") + " AS latency_bucket_2000",
		s.d.ConditionalSum("latency_ms BETWEEN 2000 AND 4999") + " AS latency_bucket_5000",
		s.d.ConditionalSum("latency_ms >= 5000") + " AS latency_bucket_slow",
	)

	// GROUP BY columns
	groupBy := []string{"org_id"}
	groupBy = append(groupBy, dimGroups...)
	groupBy = append(groupBy, statusGroupCASE(), hourTrunc, "currency")

	// ON CONFLICT target
	conflictCols := []string{
		"org_id", "agg_level",
		"COALESCE(team_id, -1)", "COALESCE(api_key_id, -1)",
		"COALESCE(provider_id, -1)", "COALESCE(model_name, '')",
		"COALESCE(route_type, '')", "status_group", "hour_bucket", "currency",
	}

	// DO UPDATE SET clause — all metric columns
	updateCols := []string{
		"request_count", "input_tokens", "output_tokens", "reasoning_tokens", "cache_read_tokens",
		"total_cost", "total_latency_ms", "min_latency_ms", "max_latency_ms", "latency_samples",
		"total_first_token_ms", "first_token_samples",
		"error_count", "fallback_count", "retry_count", "guardrail_blocks", "cache_hits",
		"distinct_sessions", "distinct_keys",
		"latency_bucket_50", "latency_bucket_100", "latency_bucket_200",
		"latency_bucket_500", "latency_bucket_1000", "latency_bucket_2000",
		"latency_bucket_5000", "latency_bucket_slow",
	}
	var setParts []string
	for _, col := range updateCols {
		setParts = append(setParts, col+" = EXCLUDED."+col)
	}

	sql := fmt.Sprintf(
		"INSERT INTO datalens_hourly_metrics (%s)\nSELECT %s\nFROM usage_logs\nWHERE created_at >= $1 AND created_at < $2 AND org_id IS NOT NULL\nGROUP BY %s\nON CONFLICT (%s) DO UPDATE SET %s",
		strings.Join(insertCols, ", "),
		strings.Join(selectExprs, ",\n"),
		strings.Join(groupBy, ", "),
		strings.Join(conflictCols, ", "),
		strings.Join(setParts, ", "),
	)

	return sql, []any{start, end}
}

// aggregateDaily runs the two-step daily aggregation.
func (s *DataLensAggregatorService) aggregateDaily(ctx context.Context) error {
	now := time.Now().UTC()
	dayEnd := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	dayStart := dayEnd.Add(-24 * time.Hour)

	// Step 1: Aggregate from hourly_metrics to daily_metrics (SUM/MIN/MAX).
	if err := s.aggregateDailyFromHourly(ctx, dayStart, dayEnd); err != nil {
		return fmt.Errorf("daily from hourly: %w", err)
	}

	// Step 2: Update distinct_sessions and distinct_keys from raw usage_logs per level.
	if err := s.updateDailyDistinctCounts(ctx, dayStart, dayEnd); err != nil {
		return fmt.Errorf("daily distinct counts: %w", err)
	}
	return nil
}

func (s *DataLensAggregatorService) aggregateDailyFromHourly(ctx context.Context, dayStart, dayEnd time.Time) error {
	dayTrunc := s.d.DateTrunc("day", "hour_bucket")

	// Non-distinct metric columns: aggregated as SUM, except min/max.
	sumCols := []string{
		"request_count", "input_tokens", "output_tokens", "reasoning_tokens", "cache_read_tokens",
		"total_cost", "total_latency_ms", "latency_samples",
		"total_first_token_ms", "first_token_samples",
		"error_count", "fallback_count", "retry_count", "guardrail_blocks", "cache_hits",
		"latency_bucket_50", "latency_bucket_100", "latency_bucket_200",
		"latency_bucket_500", "latency_bucket_1000", "latency_bucket_2000",
		"latency_bucket_5000", "latency_bucket_slow",
	}

	// Build SELECT: SUM for most, MIN for min_latency, MAX for max_latency, 0 for distinct.
	var selectExprs []string
	selectExprs = append(selectExprs,
		"org_id", "agg_level", "team_id", "api_key_id", "provider_id",
		"model_name", "route_type", "status_group",
		dayTrunc+" AS day_bucket",
		"currency",
	)
	for _, col := range sumCols {
		selectExprs = append(selectExprs, "SUM("+col+") AS "+col)
	}
	selectExprs = append(selectExprs,
		"MIN(min_latency_ms) AS min_latency_ms",
		"MAX(max_latency_ms) AS max_latency_ms",
		"0 AS distinct_sessions",
		"0 AS distinct_keys",
	)

	// INSERT column list: all dimension + metric columns.
	insertCols := []string{
		"org_id", "agg_level", "team_id", "api_key_id", "provider_id",
		"model_name", "route_type", "status_group", "day_bucket", "currency",
	}
	insertCols = append(insertCols, sumCols...)
	insertCols = append(insertCols, "min_latency_ms", "max_latency_ms", "distinct_sessions", "distinct_keys")

	// GROUP BY
	groupBy := []string{
		"org_id", "agg_level", "team_id", "api_key_id", "provider_id",
		"model_name", "route_type", "status_group",
		dayTrunc, "currency",
	}

	// ON CONFLICT
	conflictCols := []string{
		"org_id", "agg_level",
		"COALESCE(team_id, -1)", "COALESCE(api_key_id, -1)",
		"COALESCE(provider_id, -1)", "COALESCE(model_name, '')",
		"COALESCE(route_type, '')", "status_group", "day_bucket", "currency",
	}

	// DO UPDATE SET: same as hourly but using daily column names.
	updateCols := []string{
		"request_count", "input_tokens", "output_tokens", "reasoning_tokens", "cache_read_tokens",
		"total_cost", "total_latency_ms", "min_latency_ms", "max_latency_ms", "latency_samples",
		"total_first_token_ms", "first_token_samples",
		"error_count", "fallback_count", "retry_count", "guardrail_blocks", "cache_hits",
		"distinct_sessions", "distinct_keys",
		"latency_bucket_50", "latency_bucket_100", "latency_bucket_200",
		"latency_bucket_500", "latency_bucket_1000", "latency_bucket_2000",
		"latency_bucket_5000", "latency_bucket_slow",
	}
	var setParts []string
	for _, col := range updateCols {
		setParts = append(setParts, col+" = EXCLUDED."+col)
	}

	sql := fmt.Sprintf(
		"INSERT INTO datalens_daily_metrics (%s)\nSELECT %s\nFROM datalens_hourly_metrics\nWHERE hour_bucket >= $1 AND hour_bucket < $2\nGROUP BY %s\nON CONFLICT (%s) DO UPDATE SET %s",
		strings.Join(insertCols, ", "),
		strings.Join(selectExprs, ",\n"),
		strings.Join(groupBy, ", "),
		strings.Join(conflictCols, ", "),
		strings.Join(setParts, ", "),
	)

	t0 := time.Now()
	result := s.db.WithContext(ctx).Exec(sql, dayStart, dayEnd)
	elapsed := time.Since(t0)
	rows := 0
	if result.RowsAffected >= 0 {
		rows = int(result.RowsAffected)
	}
	if result.Error != nil {
		slog.Warn("datalens aggregator: daily from hourly failed", "error", result.Error, "elapsed_ms", elapsed.Milliseconds())
		return result.Error
	}
	slog.Info("datalens aggregator: daily from hourly done", "rows", rows, "elapsed_ms", elapsed.Milliseconds())
	return nil
}

// updateDailyDistinctCounts updates distinct_sessions and distinct_keys in daily_metrics
// by querying raw usage_logs per aggregation level.
func (s *DataLensAggregatorService) updateDailyDistinctCounts(ctx context.Context, dayStart, dayEnd time.Time) error {
	dayTrunc := s.d.DateTrunc("day", "created_at")

	// dimensionColumnMap maps dimension names used in AggregateLevel to usage_logs columns.
	dimensionColumnMap := map[string]string{
		"model_requested": "model_name", // in daily table, stored as model_name
		"team_id":         "team_id",
		"api_key_id":      "api_key_id",
		"provider_id":     "provider_id",
	}

	var firstErr error
	for _, level := range s.levels {
		t0 := time.Now()

		var selectCols []string
		var groupByCols []string
		var joinConds []string
		var whereExtras []string

		selectCols = append(selectCols, "sub.org_id")
		groupByCols = append(groupByCols, "sub.org_id")
		joinConds = append(joinConds, "d.org_id = sub.org_id")

		for _, dim := range level.Dimensions {
			col := dimensionColumnMap[dim]
			if col == "" {
				continue
			}
			selectCols = append(selectCols, "sub."+col)
			groupByCols = append(groupByCols, "sub."+col)
			joinConds = append(joinConds, fmt.Sprintf("d.%s = sub.%s", col, col))
			whereExtras = append(whereExtras, dim+" IS NOT NULL")
		}

		groupByCols = append(groupByCols, "sub.day_bucket", "sub.currency", "sub.status_group")
		joinConds = append(joinConds,
			"d.day_bucket = sub.day_bucket",
			"d.currency = sub.currency",
			"d.status_group = sub.status_group",
			fmt.Sprintf("d.agg_level = '%s'", level.Name),
		)

		whereClause := "created_at >= $1 AND created_at < $2 AND org_id IS NOT NULL"
		if len(whereExtras) > 0 {
			whereClause += " AND " + strings.Join(whereExtras, " AND ")
		}

		// For model_requested dim, we need to SELECT it as model_name alias to match the daily table column.
		// Build the subquery SELECT with proper aliasing.
		var subSelectParts []string
		subSelectParts = append(subSelectParts, "org_id")
		for _, dim := range level.Dimensions {
			col := dimensionColumnMap[dim]
			if dim == "model_requested" {
				subSelectParts = append(subSelectParts, "model_requested AS model_name")
			} else {
				subSelectParts = append(subSelectParts, col)
			}
		}
		subSelectParts = append(subSelectParts,
			dayTrunc+" AS day_bucket",
			"currency",
			statusGroupCASE()+" AS status_group",
			"COUNT(DISTINCT CASE WHEN session_id != '' THEN session_id END) AS sessions",
			"COUNT(DISTINCT api_key_id) AS keys",
		)

		// Build GROUP BY for subquery with proper column references.
		var subGroupBy []string
		subGroupBy = append(subGroupBy, "org_id")
		for _, dim := range level.Dimensions {
			if dim == "model_requested" {
				subGroupBy = append(subGroupBy, "model_requested")
			} else {
				col := dimensionColumnMap[dim]
				subGroupBy = append(subGroupBy, col)
			}
		}
		subGroupBy = append(subGroupBy, dayTrunc, "currency", statusGroupCASE())

		sql := fmt.Sprintf(
			"UPDATE datalens_daily_metrics d\nSET distinct_sessions = sub.sessions, distinct_keys = sub.keys\nFROM (\n  SELECT %s\n  FROM usage_logs\n  WHERE %s\n  GROUP BY %s\n) sub\nWHERE %s",
			strings.Join(subSelectParts, ", "),
			whereClause,
			strings.Join(subGroupBy, ", "),
			strings.Join(joinConds, " AND "),
		)

		result := s.db.WithContext(ctx).Exec(sql, dayStart, dayEnd)
		elapsed := time.Since(t0)
		rows := 0
		if result.RowsAffected >= 0 {
			rows = int(result.RowsAffected)
		}
		if result.Error != nil {
			slog.Warn("datalens aggregator: daily distinct counts failed",
				"level", level.Name, "error", result.Error, "elapsed_ms", elapsed.Milliseconds())
			s.updateAggStatus(ctx, level.Name, "daily_distinct", int(elapsed.Milliseconds()), rows, result.Error)
			if firstErr == nil {
				firstErr = fmt.Errorf("daily distinct %s: %w", level.Name, result.Error)
			}
			continue
		}
		s.updateAggStatus(ctx, level.Name, "daily_distinct", int(elapsed.Milliseconds()), rows, nil)
	}
	return firstErr
}

// backfill aggregates historical data for the last backfillDays days.
func (s *DataLensAggregatorService) backfill(ctx context.Context) {
	if s.backfillDays <= 0 {
		return
	}
	now := time.Now().UTC()
	days := s.backfillDays
	slog.Info("datalens aggregator: starting backfill", "days", days)

	for i := days; i >= 1; i-- {
		dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -i)
		dayEnd := dayStart.AddDate(0, 0, 1)

		select {
		case <-ctx.Done():
			slog.Info("datalens aggregator: backfill cancelled")
			return
		default:
		}

		// Hourly aggregation for this day.
		for _, level := range s.levels {
			sql, args := s.buildHourlySQL(level, dayStart, dayEnd)
			if result := s.db.WithContext(ctx).Exec(sql, args...); result.Error != nil {
				slog.Warn("datalens aggregator: backfill hourly failed",
					"level", level.Name, "day", dayStart.Format("2006-01-02"), "error", result.Error)
			}
		}

		// Daily aggregation for this day.
		if err := s.aggregateDailyFromHourly(ctx, dayStart, dayEnd); err != nil {
			slog.Warn("datalens aggregator: backfill daily failed",
				"day", dayStart.Format("2006-01-02"), "error", err)
		}
		if err := s.updateDailyDistinctCounts(ctx, dayStart, dayEnd); err != nil {
			slog.Warn("datalens aggregator: backfill daily distinct failed",
				"day", dayStart.Format("2006-01-02"), "error", err)
		}

		// Sleep between days to avoid pressure.
		time.Sleep(200 * time.Millisecond)
	}
	slog.Info("datalens aggregator: backfill complete", "days", days)
}

// cleanupRetentionPolicy removes stale pre-aggregated rows beyond the configured retention.
func (s *DataLensAggregatorService) cleanupRetentionPolicy(ctx context.Context) error {
	var firstErr error
	if s.hourlyDays > 0 {
		cutoff := time.Now().UTC().AddDate(0, 0, -s.hourlyDays)
		result := s.db.WithContext(ctx).Exec(
			"DELETE FROM datalens_hourly_metrics WHERE hour_bucket < $1", cutoff,
		)
		if result.Error != nil {
			slog.Warn("datalens aggregator: hourly cleanup failed", "error", result.Error)
			if firstErr == nil {
				firstErr = result.Error
			}
		} else if result.RowsAffected > 0 {
			slog.Info("datalens aggregator: hourly cleanup", "deleted", result.RowsAffected, "cutoff", cutoff.Format("2006-01-02"))
		}
	}
	if s.dailyDays > 0 {
		cutoff := time.Now().UTC().AddDate(0, 0, -s.dailyDays)
		result := s.db.WithContext(ctx).Exec(
			"DELETE FROM datalens_daily_metrics WHERE day_bucket < $1", cutoff,
		)
		if result.Error != nil {
			slog.Warn("datalens aggregator: daily cleanup failed", "error", result.Error)
			if firstErr == nil {
				firstErr = result.Error
			}
		} else if result.RowsAffected > 0 {
			slog.Info("datalens aggregator: daily cleanup", "deleted", result.RowsAffected, "cutoff", cutoff.Format("2006-01-02"))
		}
	}
	return firstErr
}

// updateAggStatus upserts aggregation health status for a level/type pair.
func (s *DataLensAggregatorService) updateAggStatus(ctx context.Context, level, aggType string, durationMs int, rows int, err error) {
	status := model.DataLensAggStatus{
		AggLevel:       level,
		AggType:        aggType,
		LastSuccessAt:  time.Now().UTC(),
		LastDurationMs: durationMs,
		RowsAffected:   rows,
		UpdatedAt:      time.Now().UTC(),
	}
	if err != nil {
		msg := err.Error()
		status.ErrorMessage = &msg
	}

	upsertErr := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "agg_level"},
			{Name: "agg_type"},
		},
		DoUpdates: clause.AssignmentColumns([]string{
			"last_success_at", "last_duration_ms", "rows_affected",
			"error_message", "updated_at",
		}),
	}).Create(&status).Error
	if upsertErr != nil {
		slog.Warn("datalens aggregator: failed to update agg status",
			"level", level, "agg_type", aggType, "error", upsertErr)
	}
}

// statusGroupCASE returns the SQL CASE expression for mapping status_code to status_group.
func statusGroupCASE() string {
	return `CASE WHEN status_code >= 200 AND status_code < 300 THEN 200 WHEN status_code = 429 THEN 429 WHEN status_code >= 400 AND status_code < 500 THEN 400 WHEN status_code >= 500 THEN 500 ELSE 0 END`
}
