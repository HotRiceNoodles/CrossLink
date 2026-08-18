package model

import (
	"time"

	"gorm.io/gorm"
)

// DataLensHourlyMetric is a row in the hourly pre-aggregation table.
type DataLensHourlyMetric struct {
	ID                int64     `gorm:"primaryKey" json:"id"`
	OrgID             int64     `gorm:"not null;index:idx_dhm_org_hour" json:"org_id"`
	AggLevel          string    `gorm:"size:16;not null" json:"agg_level"`
	TeamID            *int64    `gorm:"index:idx_dhm_team_hour" json:"team_id"`
	APIKeyID          *int64    `gorm:"index:idx_dhm_key_hour" json:"api_key_id"`
	ProviderID        *int64    `gorm:"index:idx_dhm_provider_hour" json:"provider_id"`
	ModelName         *string   `gorm:"size:128;index:idx_dhm_model_hour" json:"model_name"`
	RouteType         *string   `gorm:"size:16" json:"route_type"`
	StatusGroup       int       `gorm:"not null;default:200" json:"status_group"`
	HourBucket        time.Time `gorm:"not null;index:idx_dhm_org_hour" json:"hour_bucket"`
	Currency          string    `gorm:"size:3;not null;default:'CNY'" json:"currency"`
	RequestCount      int       `gorm:"not null;default:0" json:"request_count"`
	InputTokens       int64     `gorm:"not null;default:0" json:"input_tokens"`
	OutputTokens      int64     `gorm:"not null;default:0" json:"output_tokens"`
	ReasoningTokens   int64     `gorm:"not null;default:0" json:"reasoning_tokens"`
	CacheReadTokens   int64     `gorm:"not null;default:0" json:"cache_read_tokens"`
	TotalCost         float64   `gorm:"type:decimal(20,8);not null;default:0" json:"total_cost"`
	TotalLatencyMs    int64     `gorm:"not null;default:0" json:"total_latency_ms"`
	MinLatencyMs      int       `gorm:"not null;default:0" json:"min_latency_ms"`
	MaxLatencyMs      int       `gorm:"not null;default:0" json:"max_latency_ms"`
	LatencySamples    int       `gorm:"not null;default:0" json:"latency_samples"`
	FirstTokenSamples int       `gorm:"not null;default:0" json:"first_token_samples"`
	TotalFirstTokenMs int64     `gorm:"not null;default:0" json:"total_first_token_ms"`
	ErrorCount        int       `gorm:"not null;default:0" json:"error_count"`
	FallbackCount     int       `gorm:"not null;default:0" json:"fallback_count"`
	RetryCount        int       `gorm:"not null;default:0" json:"retry_count"`
	GuardrailBlocks   int       `gorm:"not null;default:0" json:"guardrail_blocks"`
	CacheHits         int       `gorm:"not null;default:0" json:"cache_hits"`
	DistinctSessions  int       `gorm:"not null;default:0" json:"distinct_sessions"`
	DistinctKeys      int       `gorm:"not null;default:0" json:"distinct_keys"`
	LatencyBucket50   int       `gorm:"column:latency_bucket_50;not null;default:0" json:"latency_bucket_50"`
	LatencyBucket100  int       `gorm:"column:latency_bucket_100;not null;default:0" json:"latency_bucket_100"`
	LatencyBucket200  int       `gorm:"column:latency_bucket_200;not null;default:0" json:"latency_bucket_200"`
	LatencyBucket500  int       `gorm:"column:latency_bucket_500;not null;default:0" json:"latency_bucket_500"`
	LatencyBucket1000 int       `gorm:"column:latency_bucket_1000;not null;default:0" json:"latency_bucket_1000"`
	LatencyBucket2000 int       `gorm:"column:latency_bucket_2000;not null;default:0" json:"latency_bucket_2000"`
	LatencyBucket5000 int       `gorm:"column:latency_bucket_5000;not null;default:0" json:"latency_bucket_5000"`
	LatencyBucketSlow int       `gorm:"not null;default:0" json:"latency_bucket_slow"`
	CtxSystemTokens       int64 `gorm:"not null;default:0" json:"ctx_system_tokens"`
	CtxHistoryTokens      int64 `gorm:"not null;default:0" json:"ctx_history_tokens"`
	CtxQuestionTokens     int64 `gorm:"not null;default:0" json:"ctx_question_tokens"`
	CtxToolTokens         int64 `gorm:"not null;default:0" json:"ctx_tool_tokens"`
	CtxToolOutputTokens   int64 `gorm:"not null;default:0" json:"ctx_tool_output_tokens"`
	CtxTotalWindow        int64 `gorm:"not null;default:0" json:"ctx_total_window"`
	CtxAnalyzedCount      int   `gorm:"not null;default:0" json:"ctx_analyzed_count"`
	CtxOverflowCount      int   `gorm:"not null;default:0" json:"ctx_overflow_count"`
	CtxWindowUnknownCount int   `gorm:"not null;default:0" json:"ctx_window_unknown_count"`
	CtxUtilBucketLt50     int   `gorm:"not null;default:0" json:"ctx_util_bucket_lt50"`
	CtxUtilBucket5080     int   `gorm:"column:ctx_util_bucket_50_80;not null;default:0" json:"ctx_util_bucket_50_80"`
	CtxUtilBucket8095     int   `gorm:"column:ctx_util_bucket_80_95;not null;default:0" json:"ctx_util_bucket_80_95"`
	CtxUtilBucketGt95     int   `gorm:"not null;default:0" json:"ctx_util_bucket_gt95"`
	CreatedAt         time.Time `gorm:"not null;default:now()" json:"created_at"`
}

func (DataLensHourlyMetric) TableName() string { return "datalens_hourly_metrics" }

// DataLensDailyMetric — same structure, day_bucket instead of hour_bucket.
type DataLensDailyMetric struct {
	ID                int64     `gorm:"primaryKey" json:"id"`
	OrgID             int64     `gorm:"not null" json:"org_id"`
	AggLevel          string    `gorm:"size:16;not null" json:"agg_level"`
	TeamID            *int64    `json:"team_id"`
	APIKeyID          *int64    `json:"api_key_id"`
	ProviderID        *int64    `json:"provider_id"`
	ModelName         *string   `gorm:"size:128" json:"model_name"`
	RouteType         *string   `gorm:"size:16" json:"route_type"`
	StatusGroup       int       `gorm:"not null;default:200" json:"status_group"`
	DayBucket         time.Time `gorm:"not null;type:date" json:"day_bucket"`
	Currency          string    `gorm:"size:3;not null;default:'CNY'" json:"currency"`
	RequestCount      int       `gorm:"not null;default:0" json:"request_count"`
	InputTokens       int64     `gorm:"not null;default:0" json:"input_tokens"`
	OutputTokens      int64     `gorm:"not null;default:0" json:"output_tokens"`
	ReasoningTokens   int64     `gorm:"not null;default:0" json:"reasoning_tokens"`
	CacheReadTokens   int64     `gorm:"not null;default:0" json:"cache_read_tokens"`
	TotalCost         float64   `gorm:"type:decimal(20,8);not null;default:0" json:"total_cost"`
	TotalLatencyMs    int64     `gorm:"not null;default:0" json:"total_latency_ms"`
	MinLatencyMs      int       `gorm:"not null;default:0" json:"min_latency_ms"`
	MaxLatencyMs      int       `gorm:"not null;default:0" json:"max_latency_ms"`
	LatencySamples    int       `gorm:"not null;default:0" json:"latency_samples"`
	FirstTokenSamples int       `gorm:"not null;default:0" json:"first_token_samples"`
	TotalFirstTokenMs int64     `gorm:"not null;default:0" json:"total_first_token_ms"`
	ErrorCount        int       `gorm:"not null;default:0" json:"error_count"`
	FallbackCount     int       `gorm:"not null;default:0" json:"fallback_count"`
	RetryCount        int       `gorm:"not null;default:0" json:"retry_count"`
	GuardrailBlocks   int       `gorm:"not null;default:0" json:"guardrail_blocks"`
	CacheHits         int       `gorm:"not null;default:0" json:"cache_hits"`
	DistinctSessions  int       `gorm:"not null;default:0" json:"distinct_sessions"`
	DistinctKeys      int       `gorm:"not null;default:0" json:"distinct_keys"`
	LatencyBucket50   int       `gorm:"column:latency_bucket_50;not null;default:0" json:"latency_bucket_50"`
	LatencyBucket100  int       `gorm:"column:latency_bucket_100;not null;default:0" json:"latency_bucket_100"`
	LatencyBucket200  int       `gorm:"column:latency_bucket_200;not null;default:0" json:"latency_bucket_200"`
	LatencyBucket500  int       `gorm:"column:latency_bucket_500;not null;default:0" json:"latency_bucket_500"`
	LatencyBucket1000 int       `gorm:"column:latency_bucket_1000;not null;default:0" json:"latency_bucket_1000"`
	LatencyBucket2000 int       `gorm:"column:latency_bucket_2000;not null;default:0" json:"latency_bucket_2000"`
	LatencyBucket5000 int       `gorm:"column:latency_bucket_5000;not null;default:0" json:"latency_bucket_5000"`
	LatencyBucketSlow int       `gorm:"not null;default:0" json:"latency_bucket_slow"`
	CtxSystemTokens       int64 `gorm:"not null;default:0" json:"ctx_system_tokens"`
	CtxHistoryTokens      int64 `gorm:"not null;default:0" json:"ctx_history_tokens"`
	CtxQuestionTokens     int64 `gorm:"not null;default:0" json:"ctx_question_tokens"`
	CtxToolTokens         int64 `gorm:"not null;default:0" json:"ctx_tool_tokens"`
	CtxToolOutputTokens   int64 `gorm:"not null;default:0" json:"ctx_tool_output_tokens"`
	CtxTotalWindow        int64 `gorm:"not null;default:0" json:"ctx_total_window"`
	CtxAnalyzedCount      int   `gorm:"not null;default:0" json:"ctx_analyzed_count"`
	CtxOverflowCount      int   `gorm:"not null;default:0" json:"ctx_overflow_count"`
	CtxWindowUnknownCount int   `gorm:"not null;default:0" json:"ctx_window_unknown_count"`
	CtxUtilBucketLt50     int   `gorm:"not null;default:0" json:"ctx_util_bucket_lt50"`
	CtxUtilBucket5080     int   `gorm:"column:ctx_util_bucket_50_80;not null;default:0" json:"ctx_util_bucket_50_80"`
	CtxUtilBucket8095     int   `gorm:"column:ctx_util_bucket_80_95;not null;default:0" json:"ctx_util_bucket_80_95"`
	CtxUtilBucketGt95     int   `gorm:"not null;default:0" json:"ctx_util_bucket_gt95"`
	CreatedAt         time.Time `gorm:"not null;default:now()" json:"created_at"`
}

func (DataLensDailyMetric) TableName() string { return "datalens_daily_metrics" }

// DataLensAggStatus tracks aggregation health per level.
type DataLensAggStatus struct {
	ID             int64     `gorm:"primaryKey" json:"id"`
	AggLevel       string    `gorm:"size:16;not null;uniqueIndex:idx_das_level_type" json:"agg_level"`
	AggType        string    `gorm:"size:8;not null;uniqueIndex:idx_das_level_type" json:"agg_type"`
	LastSuccessAt  time.Time `gorm:"not null" json:"last_success_at"`
	LastDurationMs int       `gorm:"not null;default:0" json:"last_duration_ms"`
	RowsAffected   int       `gorm:"not null;default:0" json:"rows_affected"`
	ErrorMessage   *string   `gorm:"type:text" json:"error_message,omitempty"`
	UpdatedAt      time.Time `gorm:"not null;default:now()" json:"updated_at"`
}

func (DataLensAggStatus) TableName() string { return "datalens_agg_status" }

// DataLensReport is a user-saved report configuration.
type DataLensReport struct {
	ID          int64          `gorm:"primaryKey" json:"id"`
	OrgID       int64          `gorm:"not null" json:"org_id"`
	UserID      int64          `gorm:"not null" json:"user_id"`
	Name        string         `gorm:"size:128;not null" json:"name"`
	Description *string        `gorm:"type:text" json:"description,omitempty"`
	Type        string         `gorm:"size:16;not null;default:'custom'" json:"type"`
	TemplateID  *string        `gorm:"size:64" json:"template_id,omitempty"`
	Scope       string         `gorm:"size:16;not null;default:'private'" json:"scope"`
	Config      string         `gorm:"type:jsonb;not null" json:"config"`
	IsPinned    bool           `gorm:"not null;default:false" json:"is_pinned"`
	Version     int            `gorm:"not null;default:1" json:"version"`
	CreatedAt   time.Time      `gorm:"not null;default:now()" json:"created_at"`
	UpdatedAt   time.Time      `gorm:"not null;default:now()" json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
}

func (DataLensReport) TableName() string { return "datalens_reports" }

// DataLensSchedule is an automated report delivery schedule (Enterprise).
type DataLensSchedule struct {
	ID        int64          `gorm:"primaryKey" json:"id"`
	OrgID     int64          `gorm:"not null" json:"org_id"`
	UserID    int64          `gorm:"not null" json:"user_id"`
	TeamID    *int64         `json:"team_id,omitempty"`
	Name      string         `gorm:"size:128;not null" json:"name"`
	ReportID  int64          `gorm:"not null" json:"report_id"`
	Cron      string         `gorm:"size:64;not null" json:"cron"`
	Timezone  string         `gorm:"size:32;not null;default:'Asia/Shanghai'" json:"timezone"`
	Channels  string         `gorm:"type:jsonb;not null" json:"channels"`
	Enabled   bool           `gorm:"not null;default:true" json:"enabled"`
	LastRunAt *time.Time     `json:"last_run_at,omitempty"`
	NextRunAt *time.Time     `json:"next_run_at,omitempty"`
	CreatedAt time.Time      `gorm:"not null;default:now()" json:"created_at"`
	UpdatedAt time.Time      `gorm:"not null;default:now()" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

func (DataLensSchedule) TableName() string { return "datalens_schedules" }
