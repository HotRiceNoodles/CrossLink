package repository

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/crosslink/internal/dialect"
	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// Query parameter types
// ---------------------------------------------------------------------------

// Filter represents a dimension filter condition.
type Filter struct {
	Dimension string `json:"dimension"`
	Operator  string `json:"operator"`
	Value     any    `json:"value"`
	Values    []any  `json:"values,omitempty"`
}

// TimeRange specifies the query time window.
type TimeRange struct {
	Type   string     `json:"type"`            // "preset", "relative", "absolute"
	Preset string     `json:"preset,omitempty"` // "last_7d", "last_30d", "last_90d", "this_month", "last_month"
	Start  *time.Time `json:"start,omitempty"`
	End    *time.Time `json:"end,omitempty"`
}

// QueryParams holds all parameters for a DataLens query.
type QueryParams struct {
	OrgID       int64     `json:"org_id"`
	Dimensions  []string  `json:"dimensions"`
	Metrics     []string  `json:"metrics"`
	Filters     []Filter  `json:"filters"`
	TimeRange   TimeRange `json:"time_range"`
	Granularity string    `json:"granularity"` // "hour", "day", "week", "month"
	SortBy      string    `json:"sort_by"`
	SortOrder   string    `json:"sort_order"` // "asc", "desc"
	Limit       int       `json:"limit"`
	// Accept camelCase variants from frontend — handler maps these after binding.
	FrontendTimeRange *TimeRange `json:"timeRange"`
	FrontendSortBy    string     `json:"sortBy"`
	FrontendSortOrder string     `json:"sortOrder"`
}

// ---------------------------------------------------------------------------
// Result types
// ---------------------------------------------------------------------------

// ColumnMeta describes a single column in the query result.
type ColumnMeta struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Type   string `json:"type"`            // "dimension", "time", "metric"
	Format string `json:"format,omitempty"` // "number", "currency", "ms", "percent"
}

// QueryResult is the unified response for DataLens queries.
type QueryResult struct {
	Columns []ColumnMeta     `json:"columns"`
	Rows    []map[string]any `json:"rows"`
	Total   int              `json:"total"`
	Meta    QueryMeta        `json:"meta"`
}

// QueryMeta carries metadata about the executed query.
type QueryMeta struct {
	QueryTimeMs      int    `json:"query_time_ms"`
	DataSource       string `json:"data_source"`
	Currency         string `json:"currency"`
	LastAggregatedAt string `json:"last_aggregated_at,omitempty"`
	StaleWarning     bool   `json:"stale_warning"`
}

// ---------------------------------------------------------------------------
// Drill-down types
// ---------------------------------------------------------------------------

// DrillDownParams holds parameters for a multi-dimensional drill-down query.
type DrillDownParams struct {
	OrgID      int64       `json:"org_id"`
	Path       []DrillStep `json:"path"`
	CurrentDim string      `json:"current_dim"`
	TargetDim  string      `json:"target_dim"`
	Filters    []Filter    `json:"filters"`
	Metrics    []string    `json:"metrics"`
	TimeRange  TimeRange   `json:"time_range"`
}

// DrillStep represents a single step in the drill-down breadcrumb path.
type DrillStep struct {
	Dimension string `json:"dimension"`
	Value     string `json:"value"`
}

// ---------------------------------------------------------------------------
// Aggregation status types
// ---------------------------------------------------------------------------

// AggregationStatus contains the health status of all aggregation levels.
type AggregationStatus struct {
	Levels []AggLevelStatus `json:"levels"`
}

// AggLevelStatus describes the health of a single aggregation level.
type AggLevelStatus struct {
	Level          string     `json:"level"`
	Type           string     `json:"type"`
	LastSuccessAt  *time.Time `json:"last_success_at"`
	LastDurationMs int        `json:"last_duration_ms"`
	RowsAffected   int        `json:"rows_affected"`
	ErrorMessage   *string    `json:"error_message,omitempty"`
}

// ---------------------------------------------------------------------------
// MetricsStore interface
// ---------------------------------------------------------------------------

// MetricsStore defines the BI data storage abstraction.
// Default implementation uses PostgreSQL pre-aggregation tables;
// future implementations may target ClickHouse or other column stores.
type MetricsStore interface {
	// Query executes a pre-aggregation query.
	Query(ctx context.Context, params QueryParams) (*QueryResult, error)

	// QueryDrillDown translates a drill-down path into filters and delegates to Query.
	QueryDrillDown(ctx context.Context, params DrillDownParams) (*QueryResult, error)

	// GetTimeRange returns the earliest and latest data timestamps for an org.
	GetTimeRange(ctx context.Context, orgID int64) (start, end time.Time, err error)

	// GetAggStatus returns aggregation health for all levels.
	GetAggStatus(ctx context.Context) (*AggregationStatus, error)
}

// ---------------------------------------------------------------------------
// Dimension whitelist — maps safe dimension names to actual column names.
// This prevents SQL injection: only whitelisted dimensions are allowed in
// GROUP BY, WHERE, and ORDER BY clauses.
// ---------------------------------------------------------------------------

// queryableDimensions lists dimensions that produce meaningful results from
// pre-aggregated tables. Dimensions like route/error_type/agent are present
// in dimensionColumnMap (for filter SQL mapping) but are always NULL in the
// pre-agg tables, so querying by them silently returns empty results.
var queryableDimensions = map[string]bool{
	"model":    true,
	"team":     true,
	"key":      true,
	"provider": true,
	"status":   true,
	"currency": true,
}

var dimensionColumnMap = map[string]string{
	"model":      "model_name",
	"team":       "team_id",
	"key":        "api_key_id",
	"provider":   "provider_id",
	"route":      "route_type",
	"status":     "status_group",
	"error_type": "error_type",
	"agent":      "agent_type",
	"currency":   "currency",
}

// allowedOperators is the set of valid filter operators.
var allowedOperators = map[string]bool{
	"eq": true, "neq": true, "gt": true, "gte": true,
	"lt": true, "lte": true, "in": true, "not_in": true,
	"between": true, "is_null": true, "is_not_null": true,
}

// metricExprs maps metric IDs to their SQL expressions.
// Cost metrics use conditional aggregation on the primary currency;
// non-cost metrics aggregate all currency rows.
type metricDef struct {
	Expr            string        // SQL expression template — may contain {currency} placeholder
	IsCost          bool          // true if this metric needs currency filtering
	Label           string        // human-readable label
	Format          string        // format hint for the column metadata
	IsEnterprise    bool          // true if this metric requires Enterprise tier
	NeedsComparison bool          // true if this metric needs a period-over-period comparison query
	ComparisonMetric string       // base metric to query for comparison (e.g. "cost")
	ComparisonShift  time.Duration // how far back to shift for the comparison period; 0 = use current range duration
}

// metricDefinitions is populated lazily by initMetricDefs (once).
var metricDefinitions map[string]metricDef
var metricDefsOnce sync.Once

func initMetricDefs() {
	metricDefinitions = map[string]metricDef{
		"requests": {
			Expr:   "SUM(request_count)",
			Label:  "Requests",
			Format: "number",
		},
		"cost": {
			Expr:   "SUM(CASE WHEN currency = '{currency}' THEN total_cost ELSE 0 END)",
			IsCost: true,
			Label:  "Cost",
			Format: "currency",
		},
		"input_tokens": {
			Expr:   "SUM(input_tokens)",
			Label:  "Input Tokens",
			Format: "number",
		},
		"output_tokens": {
			Expr:   "SUM(output_tokens)",
			Label:  "Output Tokens",
			Format: "number",
		},
		"total_tokens": {
			Expr:   "SUM(input_tokens + output_tokens)",
			Label:  "Total Tokens",
			Format: "number",
		},
		"reasoning_tokens": {
			Expr:   "SUM(reasoning_tokens)",
			Label:  "Reasoning Tokens",
			Format: "number",
		},
		"cache_read_tokens": {
			Expr:   "SUM(cache_read_tokens)",
			Label:  "Cache Read Tokens",
			Format: "number",
		},
		"avg_latency": {
			Expr:   "CASE WHEN SUM(latency_samples) > 0 THEN SUM(total_latency_ms) / SUM(latency_samples) ELSE 0 END",
			Label:  "Avg Latency",
			Format: "ms",
		},
		"cost_per_1k": {
			Expr:   "CASE WHEN SUM(input_tokens + output_tokens) > 0 THEN SUM(CASE WHEN currency = '{currency}' THEN total_cost ELSE 0 END) / SUM(input_tokens + output_tokens) * 1000 ELSE 0 END",
			IsCost: true,
			Label:  "Cost per 1K Tokens",
			Format: "currency",
		},
		"cost_per_request": {
			Expr:   "CASE WHEN SUM(request_count) > 0 THEN SUM(CASE WHEN currency = '{currency}' THEN total_cost ELSE 0 END) / SUM(request_count) ELSE 0 END",
			IsCost: true,
			Label:  "Cost per Request",
			Format: "currency",
		},
		"error_rate": {
			Expr:   "CASE WHEN SUM(request_count) > 0 THEN CAST(SUM(error_count) AS DECIMAL) / SUM(request_count) ELSE 0 END",
			Label:  "Error Rate",
			Format: "percent",
		},
		"fallback_rate": {
			Expr:   "CASE WHEN SUM(request_count) > 0 THEN CAST(SUM(fallback_count) AS DECIMAL) / SUM(request_count) ELSE 0 END",
			Label:  "Fallback Rate",
			Format: "percent",
		},
		"retry_rate": {
			Expr:   "CASE WHEN SUM(request_count) > 0 THEN CAST(SUM(retry_count) AS DECIMAL) / SUM(request_count) ELSE 0 END",
			Label:  "Retry Rate",
			Format: "percent",
		},
		"guardrail_rate": {
			Expr:   "CASE WHEN SUM(request_count) > 0 THEN CAST(SUM(guardrail_blocks) AS DECIMAL) / SUM(request_count) ELSE 0 END",
			Label:  "Guardrail Block Rate",
			Format: "percent",
		},
		"cache_hit_rate": {
			Expr:   "CASE WHEN SUM(request_count) > 0 THEN CAST(SUM(cache_hits) AS DECIMAL) / SUM(request_count) ELSE 0 END",
			Label:  "Cache Hit Rate",
			Format: "percent",
		},
		"avg_ttft": {
			Expr:   "CASE WHEN SUM(first_token_samples) > 0 THEN SUM(total_first_token_ms) / SUM(first_token_samples) ELSE 0 END",
			Label:  "Avg TTFT",
			Format: "ms",
		},
		"io_ratio": {
			Expr:         "CASE WHEN SUM(output_tokens) > 0 THEN CAST(SUM(input_tokens) AS DECIMAL) / SUM(output_tokens) ELSE 0 END",
			Label:        "I/O Ratio",
			Format:       "number",
			IsEnterprise: true,
		},
		"cost_change_pct": {
			Expr:             "0", // placeholder — computed post-query
			Label:            "Cost WoW Change",
			Format:           "percent",
			IsEnterprise:     true,
			NeedsComparison:  true,
			ComparisonMetric: "cost",
		},
		"cost_yoy_pct": {
			Expr:             "0", // placeholder
			Label:            "Cost YoY Change",
			Format:           "percent",
			IsEnterprise:     true,
			NeedsComparison:  true,
			ComparisonMetric: "cost",
			ComparisonShift:  365 * 24 * time.Hour,
		},
	}
}

func getMetricDefs() map[string]metricDef {
	metricDefsOnce.Do(initMetricDefs)
	return metricDefinitions
}

// ---------------------------------------------------------------------------
// PgMetricsStore — PostgreSQL implementation of MetricsStore
// ---------------------------------------------------------------------------

// PgMetricsStore implements MetricsStore backed by PostgreSQL pre-aggregation tables.
type PgMetricsStore struct {
	db       *gorm.DB
	d        dialect.Dialect
	dimCache sync.Map // dimension ID→name cache: "team:123" → "Engineering"
}

// NewPgMetricsStore creates a new PgMetricsStore.
func NewPgMetricsStore(db *gorm.DB, d dialect.Dialect) *PgMetricsStore {
	return &PgMetricsStore{db: db, d: d}
}

// ---------------------------------------------------------------------------
// Query — core SQL-building logic
// ---------------------------------------------------------------------------

// Query builds and executes a pre-aggregation query.
func (s *PgMetricsStore) Query(ctx context.Context, params QueryParams) (*QueryResult, error) {
	// Backend-side timeout safety net — don't hang beyond 15s even if client is slow.
	queryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	start := time.Now()
	slog.Info("datalens store.Query entered", "orgID", params.OrgID, "dimensions", params.Dimensions, "metrics", params.Metrics)

	// 1. Validate inputs.
	if err := validateQueryParams(&params); err != nil {
		return nil, err
	}

	// 2. Resolve time range to concrete start/end.
	rangeStart, rangeEnd, err := resolveTimeRange(&params.TimeRange)
	if err != nil {
		return nil, fmt.Errorf("resolve time range: %w", err)
	}

	// 3. Select target table based on granularity.
	table := "datalens_daily_metrics"
	bucketCol := "day_bucket"
	if params.Granularity == "hour" {
		table = "datalens_hourly_metrics"
		bucketCol = "hour_bucket"
	}

	// 3b. Resolve aggregation level early - needed by primaryCurrency and WHERE.
	aggLevel := resolveAggLevel(params.Dimensions)
	slog.Info("datalens query: resolved agg level", "aggLevel", aggLevel, "table", table,
		"dimensions", params.Dimensions, "timeMs", time.Since(start).Milliseconds())

	// 4. Determine primary currency for conditional cost aggregation.
	// Only needed when at least one requested metric uses the {currency} placeholder.
	defs := getMetricDefs()
	needsCurrency := false
	for _, m := range params.Metrics {
		if d, ok := defs[m]; ok && d.IsCost {
			needsCurrency = true
			break
		}
	}
	primaryCurrency := ""
	if needsCurrency {
		hasCurrencyFilter := false
		for _, f := range params.Filters {
			if f.Dimension == "currency" {
				hasCurrencyFilter = true
				if str, ok := f.Value.(string); ok {
					primaryCurrency = str
				}
				break
			}
		}
		if !hasCurrencyFilter {
			cur, err := s.primaryCurrency(queryCtx, params.OrgID, table, bucketCol, aggLevel, rangeStart, rangeEnd)
			if err != nil {
				return nil, fmt.Errorf("detect primary currency: %w", err)
			}
			primaryCurrency = cur
		}
	}
	if primaryCurrency == "" {
		primaryCurrency = "CNY"
	}
	slog.Info("datalens query: currency resolved", "currency", primaryCurrency,
		"needsCurrency", needsCurrency, "timeMs", time.Since(start).Milliseconds())

	// 5. Build SELECT columns.
	var selectCols []string
	var groupCols []string
	var columnMeta []ColumnMeta

	// Time bucket column (always present).
	timeExpr := s.d.DateTrunc(params.Granularity, bucketCol)
	selectCols = append(selectCols, timeExpr+" AS time_bucket")
	groupCols = append(groupCols, timeExpr)
	columnMeta = append(columnMeta, ColumnMeta{
		Key: "time_bucket", Label: "Time", Type: "time",
	})

	// Filter out the "time" pseudo-dimension — it's already emitted as time_bucket above.
	dims := make([]string, 0, len(params.Dimensions))
	for _, d := range params.Dimensions {
		if d != "time" {
			dims = append(dims, d)
		}
	}

	// Dimension columns (whitelisted).
	for _, dim := range dims {
		col, ok := dimensionColumnMap[dim]
		if !ok {
			return nil, fmt.Errorf("unknown dimension: %s", dim)
		}
		selectCols = append(selectCols, col)
		groupCols = append(groupCols, col)
		columnMeta = append(columnMeta, ColumnMeta{
			Key: dim, Label: dimLabel(dim), Type: "dimension",
		})
	}

	// Metric columns.
	for _, m := range params.Metrics {
		def, ok := defs[m]
		if !ok {
			return nil, fmt.Errorf("unknown metric: %s", m)
		}
		expr := strings.ReplaceAll(def.Expr, "{currency}", primaryCurrency)
		selectCols = append(selectCols, expr+" AS "+m)
		columnMeta = append(columnMeta, ColumnMeta{
			Key: m, Label: def.Label, Type: "metric", Format: def.Format,
		})
	}

		// 6. Build WHERE clause.
		var whereConds []string
		var whereArgs []any

		whereConds = append(whereConds, "org_id = ?")
		whereArgs = append(whereArgs, params.OrgID)

		// Filter by agg_level to avoid summing across all 7 aggregation levels.
		whereConds = append(whereConds, "agg_level = ?")
		whereArgs = append(whereArgs, aggLevel)

		whereConds = append(whereConds, bucketCol+" >= ?")
		whereArgs = append(whereArgs, rangeStart)

		whereConds = append(whereConds, bucketCol+" < ?")
		whereArgs = append(whereArgs, rangeEnd)

	for _, f := range params.Filters {
		col, ok := dimensionColumnMap[f.Dimension]
		if !ok {
			continue // skip unknown dimensions (already validated above)
		}
		cond, args, err := buildFilterCondition(col, f)
		if err != nil {
			return nil, fmt.Errorf("filter %s: %w", f.Dimension, err)
		}
		if cond != "" {
			whereConds = append(whereConds, cond)
			whereArgs = append(whereArgs, args...)
		}
	}

	// 7. Build full SQL.
	sql := fmt.Sprintf("SELECT %s FROM %s WHERE %s",
		strings.Join(selectCols, ", "),
		table,
		strings.Join(whereConds, " AND "),
	)

	if len(groupCols) > 0 {
		sql += " GROUP BY " + strings.Join(groupCols, ", ")
	}

	// ORDER BY
	if params.SortBy != "" {
		dir := "ASC"
		if strings.EqualFold(params.SortOrder, "desc") {
			dir = "DESC"
		}
		sortCol := params.SortBy
		if col, ok := dimensionColumnMap[params.SortBy]; ok {
			sortCol = col
		}
		sql += fmt.Sprintf(" ORDER BY %s %s", sortCol, dir)
	}

	// LIMIT
	if params.Limit > 0 {
		sql += fmt.Sprintf(" LIMIT %d", params.Limit)
	}

	// 8. Execute.
	slog.Info("datalens query: executing main SQL", "table", table, "aggLevel", aggLevel, "timeMs", time.Since(start).Milliseconds())
	slog.Debug("datalens query: SQL detail", "sql", sql, "args", whereArgs)
	var rows []map[string]any
	if err := s.db.WithContext(queryCtx).Raw(sql, whereArgs...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("execute query: %w", err)
	}
	slog.Debug("datalens query: main SQL done", "rows", len(rows), "timeMs", time.Since(start).Milliseconds())

	// 8b. Period-over-period comparison for Enterprise metrics.
	if err := s.computeComparisonMetrics(queryCtx, params, defs, table, bucketCol, aggLevel, primaryCurrency,
		rangeStart, rangeEnd, selectCols, whereArgs, whereConds, rows); err != nil {
		return nil, fmt.Errorf("compute comparison metrics: %w", err)
	}
	slog.Debug("datalens query: comparison metrics done", "timeMs", time.Since(start).Milliseconds())

	// 9. Resolve dimension names (ID → display name).
	if err := s.resolveDimensionNames(queryCtx, rows, params.Dimensions); err != nil {
		return nil, fmt.Errorf("resolve dimension names: %w", err)
	}
	slog.Debug("datalens query: dimension names resolved", "timeMs", time.Since(start).Milliseconds())

	// 10. Build aggregation status metadata.
	meta := QueryMeta{
		QueryTimeMs: int(time.Since(start).Milliseconds()),
		DataSource:  table,
		Currency:    primaryCurrency,
	}
	s.enrichMeta(queryCtx, &meta)

	return &QueryResult{
		Columns: columnMeta,
		Rows:    rows,
		Total:   len(rows),
		Meta:    meta,
	}, nil
}

// QueryDrillDown translates the drill-down path into QueryParams filters
// and delegates to Query.
func (s *PgMetricsStore) QueryDrillDown(ctx context.Context, params DrillDownParams) (*QueryResult, error) {
	qp := QueryParams{
		OrgID:      params.OrgID,
		Dimensions: []string{params.TargetDim},
		Metrics:    params.Metrics,
		TimeRange:  params.TimeRange,
	}

	// Convert drill steps into filters.
	for _, step := range params.Path {
		qp.Filters = append(qp.Filters, Filter{
			Dimension: step.Dimension,
			Operator:  "eq",
			Value:     step.Value,
		})
	}

	// Append any additional filters from the drill-down request.
	qp.Filters = append(qp.Filters, params.Filters...)

	// Default granularity: day.
	if qp.Granularity == "" {
		qp.Granularity = "day"
	}

	return s.Query(ctx, qp)
}

// GetTimeRange returns the earliest and latest data timestamps for an org.
func (s *PgMetricsStore) GetTimeRange(ctx context.Context, orgID int64) (start, end time.Time, err error) {
	var minVal, maxVal time.Time
	row := s.db.WithContext(ctx).Raw(
		"SELECT MIN(hour_bucket), MAX(hour_bucket) FROM datalens_hourly_metrics WHERE org_id = ?",
		orgID,
	).Row()
	if err := row.Scan(&minVal, &maxVal); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("scan time range: %w", err)
	}
	return minVal, maxVal, nil
}

// GetAggStatus returns aggregation health for all levels.
func (s *PgMetricsStore) GetAggStatus(ctx context.Context) (*AggregationStatus, error) {
	var statuses []model.DataLensAggStatus
	if err := s.db.WithContext(ctx).Order("agg_level, agg_type").Find(&statuses).Error; err != nil {
		return nil, fmt.Errorf("query agg status: %w", err)
	}

	levels := make([]AggLevelStatus, 0, len(statuses))
	for _, s := range statuses {
		t := s.LastSuccessAt
		levels = append(levels, AggLevelStatus{
			Level:          s.AggLevel,
			Type:           s.AggType,
			LastSuccessAt:  &t,
			LastDurationMs: s.LastDurationMs,
			RowsAffected:   s.RowsAffected,
			ErrorMessage:   s.ErrorMessage,
		})
	}

	return &AggregationStatus{Levels: levels}, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// primaryCurrency returns the currency with the highest total_cost in the given
// time range, used for conditional cost aggregation when no currency filter
// is specified by the caller.
func (s *PgMetricsStore) primaryCurrency(ctx context.Context, orgID int64, table, bucketCol, aggLevel string, start, end time.Time) (string, error) {
	var result struct {
		Currency string
	}
	err := s.db.WithContext(ctx).Raw(
		fmt.Sprintf(
			"SELECT currency FROM %s WHERE org_id = ? AND agg_level = ? AND %s >= ? AND %s < ? GROUP BY currency ORDER BY SUM(total_cost) DESC LIMIT 1",
			table, bucketCol, bucketCol,
		),
		orgID, aggLevel, start, end,
	).Scan(&result).Error
	if err != nil {
		return "", err
	}
	return result.Currency, nil
}

// resolveDimensionNames replaces numeric IDs with display names for
// team_id, api_key_id, and provider_id columns.
func (s *PgMetricsStore) resolveDimensionNames(ctx context.Context, rows []map[string]any, dimensions []string) error {
	for _, dim := range dimensions {
		switch dim {
		case "team":
			if err := s.resolveDimIDs(ctx, rows, "team", "teams", "id", "display_name"); err != nil {
				return err
			}
		case "key":
			if err := s.resolveDimIDs(ctx, rows, "key", "api_keys", "id", "name"); err != nil {
				return err
			}
		case "provider":
			if err := s.resolveDimIDs(ctx, rows, "provider", "providers", "id", "display_name"); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveDimIDs looks up display names for a single dimension column,
// using an in-memory cache with 5-minute TTL.
func (s *PgMetricsStore) resolveDimIDs(ctx context.Context, rows []map[string]any, dim, table, idCol, nameCol string) error {
	// Collect unique IDs that need lookup.
	colKey := dimensionColumnMap[dim] // e.g. "team_id"
	var toLookup []any
	seen := map[any]bool{}
	for _, row := range rows {
		id, ok := row[colKey]
		if !ok || id == nil {
			continue
		}
		if !seen[id] {
			seen[id] = true

			// Check cache first.
			cacheKey := fmt.Sprintf("%s:%v", dim, id)
			if cached, ok := s.dimCache.Load(cacheKey); ok {
				entry := cached.(dimCacheEntry)
				if time.Since(entry.cachedAt) < 5*time.Minute {
					continue
				}
				s.dimCache.Delete(cacheKey)
			}
			toLookup = append(toLookup, id)
		}
	}
	if len(toLookup) == 0 {
		return nil
	}

	// Batch lookup from DB.
	type nameRow struct {
		ID   int64
		Name string
	}
	var nameRows []nameRow
	err := s.db.WithContext(ctx).Raw(
		fmt.Sprintf("SELECT %s AS id, %s AS name FROM %s WHERE %s IN (?) AND deleted_at IS NULL",
			idCol, nameCol, table, idCol),
		toLookup,
	).Scan(&nameRows).Error
	if err != nil {
		return err
	}

	// Populate cache.
	nameMap := map[int64]string{}
	for _, nr := range nameRows {
		nameMap[nr.ID] = nr.Name
		cacheKey := fmt.Sprintf("%s:%d", dim, nr.ID)
		s.dimCache.Store(cacheKey, dimCacheEntry{name: nr.Name, cachedAt: time.Now()})
	}

	// Replace IDs with names in result rows.
	for _, row := range rows {
		id, ok := row[colKey]
		if !ok || id == nil {
			continue
		}
		var idInt int64
		switch v := id.(type) {
		case int64:
			idInt = v
		case float64:
			idInt = int64(v)
		default:
			continue
		}
		if name, found := nameMap[idInt]; found {
			row[dim] = name
		}
	}
	return nil
}

// dimCacheEntry stores a cached dimension name with its TTL timestamp.
type dimCacheEntry struct {
	name     string
	cachedAt time.Time
}

// enrichMeta populates QueryMeta with lastAggregatedAt and staleWarning.
func (s *PgMetricsStore) enrichMeta(ctx context.Context, meta *QueryMeta) {
	var statuses []model.DataLensAggStatus
	if err := s.db.WithContext(ctx).Find(&statuses).Error; err != nil || len(statuses) == 0 {
		return
	}

	// Find the earliest last_success_at across all levels.
	var earliest time.Time
	for _, st := range statuses {
		if earliest.IsZero() || st.LastSuccessAt.Before(earliest) {
			earliest = st.LastSuccessAt
		}
	}
	if !earliest.IsZero() {
		meta.LastAggregatedAt = earliest.UTC().Format(time.RFC3339)
		// Stale if last aggregation was more than 2 hours ago.
		if time.Since(earliest) > 2*time.Hour {
			meta.StaleWarning = true
		}
	}
}

// computeComparisonMetrics handles period-over-period Enterprise metrics.
// For each requested metric with NeedsComparison=true, it executes a second
// query over the previous period, matches rows by dimension values, and
// computes the percentage change.
func (s *PgMetricsStore) computeComparisonMetrics(
	ctx context.Context,
	params QueryParams,
	defs map[string]metricDef,
	table, bucketCol, aggLevel, primaryCurrency string,
	rangeStart, rangeEnd time.Time,
	selectCols []string,
	whereArgs []any,
	whereConds []string,
	rows []map[string]any,
) error {
	// Collect comparison metrics that were actually requested.
	type comparisonSpec struct {
		metricKey        string
		baseMetric       string
		shift            time.Duration
	}
	var comparisons []comparisonSpec
	for _, m := range params.Metrics {
		def := defs[m]
		if def.NeedsComparison {
			comparisons = append(comparisons, comparisonSpec{
				metricKey:  m,
				baseMetric: def.ComparisonMetric,
				shift:      def.ComparisonShift,
			})
		}
	}
	if len(comparisons) == 0 {
		return nil
	}

	duration := rangeEnd.Sub(rangeStart)

	for _, comp := range comparisons {
		// Determine previous period range.
		var prevStart, prevEnd time.Time
		if comp.shift > 0 {
			// Fixed shift (e.g. YoY = 365 days).
			prevStart = rangeStart.Add(-comp.shift)
			prevEnd = rangeStart
		} else {
			// WoW: shift back by the current range duration.
			prevStart = rangeStart.Add(-duration)
			prevEnd = rangeStart
		}

		slog.Debug("datalens comparison query",
			"metric", comp.metricKey,
			"prev_start", prevStart,
			"prev_end", prevEnd,
		)

		// Build a minimal query for just the base metric over the previous period.
		baseDef, ok := defs[comp.baseMetric]
		if !ok {
			return fmt.Errorf("comparison base metric %q not found", comp.baseMetric)
		}
		baseExpr := strings.ReplaceAll(baseDef.Expr, "{currency}", primaryCurrency)

		var prevSelectCols []string
		var prevGroupCols []string

		// Dimension columns (same as main query — skip "time" pseudo-dimension).
		for _, dim := range params.Dimensions {
			if dim == "time" {
				continue
			}
			col := dimensionColumnMap[dim]
			prevSelectCols = append(prevSelectCols, col)
			prevGroupCols = append(prevGroupCols, col)
		}
		prevSelectCols = append(prevSelectCols, baseExpr+" AS "+comp.baseMetric)

		// Build WHERE for previous period — clone conditions, replace time bounds.
		var prevWhereConds []string
		var prevWhereArgs []any
		prevWhereConds = append(prevWhereConds, "org_id = ?")
		prevWhereArgs = append(prevWhereArgs, params.OrgID)
		prevWhereConds = append(prevWhereConds, "agg_level = ?")
		prevWhereArgs = append(prevWhereArgs, aggLevel)
		prevWhereConds = append(prevWhereConds, bucketCol+" >= ?")
		prevWhereArgs = append(prevWhereArgs, prevStart)
		prevWhereConds = append(prevWhereConds, bucketCol+" < ?")
		prevWhereArgs = append(prevWhereArgs, prevEnd)

		// Copy dimension filters (skip currency — already resolved).
		for _, f := range params.Filters {
			col, ok := dimensionColumnMap[f.Dimension]
			if !ok {
				continue
			}
			cond, args, err := buildFilterCondition(col, f)
			if err != nil {
				return err
			}
			if cond != "" {
				prevWhereConds = append(prevWhereConds, cond)
				prevWhereArgs = append(prevWhereArgs, args...)
			}
		}

		prevSQL := fmt.Sprintf("SELECT %s FROM %s WHERE %s",
			strings.Join(prevSelectCols, ", "),
			table,
			strings.Join(prevWhereConds, " AND "),
		)
		if len(prevGroupCols) > 0 {
			prevSQL += " GROUP BY " + strings.Join(prevGroupCols, ", ")
		}

		var prevRows []map[string]any
		if err := s.db.WithContext(ctx).Raw(prevSQL, prevWhereArgs...).Scan(&prevRows).Error; err != nil {
			return fmt.Errorf("execute comparison query for %s: %w", comp.metricKey, err)
		}

		// Build lookup from dimension values → previous period value.
		prevLookup := make(map[string]float64)
		for _, pr := range prevRows {
			key := dimensionRowKey(pr, params.Dimensions)
			prevLookup[key] = toFloat64(pr[comp.baseMetric])
		}

		// Compute percentage change and add to each row.
		for _, row := range rows {
			key := dimensionRowKey(row, params.Dimensions)
			prevVal := prevLookup[key]
			curVal := toFloat64(row[comp.baseMetric])
			if prevVal != 0 {
				row[comp.metricKey] = (curVal - prevVal) / prevVal * 100
			} else {
				row[comp.metricKey] = float64(0)
			}
		}
	}
	return nil
}

// dimensionRowKey builds a composite key from dimension column values in a row.
func dimensionRowKey(row map[string]any, dimensions []string) string {
	parts := make([]string, 0, len(dimensions))
	for _, dim := range dimensions {
		if dim == "time" {
			continue
		}
		col := dimensionColumnMap[dim]
		v := row[col]
		if v == nil {
			parts = append(parts, "")
		} else {
			parts = append(parts, fmt.Sprintf("%v", v))
		}
	}
	return strings.Join(parts, "|")
}

// toFloat64 converts a numeric value from GORM's map[string]any scan to float64.
func toFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// Validation & SQL helpers (package-level)
// ---------------------------------------------------------------------------

// validateQueryParams enforces query complexity limits and input safety.
func validateQueryParams(p *QueryParams) error {
	if len(p.Dimensions) > 3 {
		return fmt.Errorf("too many dimensions (max 3, got %d)", len(p.Dimensions))
	}
	if len(p.Metrics) > 6 {
		return fmt.Errorf("too many metrics (max 6, got %d)", len(p.Metrics))
	}
	if len(p.Filters) > 10 {
		return fmt.Errorf("too many filters (max 10, got %d)", len(p.Filters))
	}
	if p.Limit > 10000 {
		return fmt.Errorf("limit too large (max 10000, got %d)", p.Limit)
	}

	// Validate dimension names against whitelist.
	// "time" is a pseudo-dimension — it's always present as time_bucket, so skip it.
	for _, d := range p.Dimensions {
		if d == "time" {
			continue
		}
		if _, ok := dimensionColumnMap[d]; !ok {
			return fmt.Errorf("unknown dimension: %s", d)
		}
		if !queryableDimensions[d] {
			return fmt.Errorf("dimension %q is not available in pre-aggregated data", d)
		}
	}

	// Validate filter dimensions and operators.
	for _, f := range p.Filters {
		if _, ok := dimensionColumnMap[f.Dimension]; !ok {
			return fmt.Errorf("unknown filter dimension: %s", f.Dimension)
		}
		if !allowedOperators[f.Operator] {
			return fmt.Errorf("invalid filter operator: %s", f.Operator)
		}
	}
	return nil
}

// resolveTimeRange converts a TimeRange (preset/relative/absolute) to concrete times.
func resolveTimeRange(tr *TimeRange) (start, end time.Time, err error) {
	now := time.Now().UTC()

	if tr.Type == "absolute" && tr.Start != nil && tr.End != nil {
		return *tr.Start, *tr.End, nil
	}

	if tr.Type == "preset" || tr.Type == "" {
		switch tr.Preset {
		case "last_7d":
			return now.AddDate(0, 0, -7), now, nil
		case "last_30d":
			return now.AddDate(0, 0, -30), now, nil
		case "last_90d":
			return now.AddDate(0, 0, -90), now, nil
		case "this_month":
			startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
			return startOfMonth, now, nil
		case "last_month":
			startOfLastMonth := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
			endOfLastMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
			return startOfLastMonth, endOfLastMonth, nil
		default:
			// Default to last 30 days if no preset specified.
			return now.AddDate(0, 0, -30), now, nil
		}
	}

	return now.AddDate(0, 0, -30), now, nil
}

// buildFilterCondition constructs a parameterized SQL condition for a filter.
// Returns the condition string and arguments, or an error for unsupported operators.
func buildFilterCondition(col string, f Filter) (string, []any, error) {
	switch f.Operator {
	case "eq":
		return col + " = ?", []any{f.Value}, nil
	case "neq":
		return col + " != ?", []any{f.Value}, nil
	case "gt":
		return col + " > ?", []any{f.Value}, nil
	case "gte":
		return col + " >= ?", []any{f.Value}, nil
	case "lt":
		return col + " < ?", []any{f.Value}, nil
	case "lte":
		return col + " <= ?", []any{f.Value}, nil
	case "in":
		if len(f.Values) == 0 {
			return "", nil, nil
		}
		placeholders := strings.Repeat("?,", len(f.Values))
		placeholders = placeholders[:len(placeholders)-1] // trim trailing comma
		return col + " IN (" + placeholders + ")", f.Values, nil
	case "not_in":
		if len(f.Values) == 0 {
			return "", nil, nil
		}
		placeholders := strings.Repeat("?,", len(f.Values))
		placeholders = placeholders[:len(placeholders)-1]
		return col + " NOT IN (" + placeholders + ")", f.Values, nil
	case "between":
		if len(f.Values) < 2 {
			return "", nil, nil
		}
		return col + " BETWEEN ? AND ?", []any{f.Values[0], f.Values[1]}, nil
	case "is_null":
		return col + " IS NULL", nil, nil
	case "is_not_null":
		return col + " IS NOT NULL", nil, nil
	default:
		return "", nil, fmt.Errorf("unsupported operator: %s", f.Operator)
	}
}

// dimLabel returns a human-readable label for a dimension key.
func dimLabel(dim string) string {
	switch dim {
	case "model":
		return "Model"
	case "team":
		return "Team"
	case "key":
		return "API Key"
	case "provider":
		return "Provider"
	case "route":
		return "Route"
	case "status":
		return "Status"
	case "error_type":
		return "Error Type"
	case "agent":
		return "Agent"
	case "currency":
		return "Currency"
	default:
		return dim
	}
}

// resolveAggLevel picks the best pre-aggregation level for the given dimensions.
// The pre-agg tables contain 7 levels: global, by_model, by_team, by_provider,
// by_key, team_model, key_model. We must query exactly one level to avoid
// inflating metrics by summing across all levels.
func resolveAggLevel(dims []string) string {
	if len(dims) == 0 {
		return "global"
	}
	has := make(map[string]bool, len(dims))
	for _, d := range dims {
		has[d] = true
	}
	switch {
	case has["model"] && has["key"]:
		return "key_model"
	case has["model"] && has["team"]:
		return "team_model"
	case has["model"]:
		return "by_model"
	case has["team"]:
		return "by_team"
	case has["provider"]:
		return "by_provider"
	case has["key"]:
		return "by_key"
	default:
		return "global"
	}
}
