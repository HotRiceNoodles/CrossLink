package model

import (
	"time"
)

// 行为基线与自适应防护 M1 表模型（docs/plans/2026-11-19-behavior-baseline-defense.md，冻结 v4.0）。
// 四表由 migrations/{mysql,postgres,sqlite} 的 behavior_baseline 迁移建立；GORM 侧只做查询/写入映射，
// 不参与 AutoMigrate。

// BehaviorRollup 是基线聚合的事实表（D8）：每实体×指标×时间桶一行，幂等 upsert
// （唯一键 entity_type+entity_id+metric+bucket_ts）。granularity 区分小时桶（35 天保留）
// 与日桶（B 类 log₂ 直方图，180 天保留）。
type BehaviorRollup struct {
	ID          int64     `gorm:"primaryKey" json:"id"`
	EntityType  string    `gorm:"size:16;not null;uniqueIndex:idx_behavior_rollups_upsert,priority:1" json:"entity_type"` // "key" | "org"
	EntityID    int64     `gorm:"not null;uniqueIndex:idx_behavior_rollups_upsert,priority:2" json:"entity_id"`
	Metric      string    `gorm:"size:128;not null;uniqueIndex:idx_behavior_rollups_upsert,priority:3" json:"metric"`
	Granularity string    `gorm:"size:4;not null;default:hour" json:"granularity"`                              // "hour" | "day"
	BucketTs    time.Time `gorm:"not null;uniqueIndex:idx_behavior_rollups_upsert,priority:4" json:"bucket_ts"` // UTC 桶起点
	ValueNum    *float64  `json:"value_num,omitempty"`                                                          // A 类求和/计数；C 类分子
	ValueDen    *float64  `json:"value_den,omitempty"`                                                          // C 类分母；A/B 类 NULL
	ValueHist   *string   `json:"value_hist,omitempty"`                                                         // B 类稀疏 log₂ 直方图 JSON [[bin,count],...]
	SampleCount int64     `gorm:"not null;default:0" json:"sample_count"`
	UpdatedAt   time.Time `gorm:"not null" json:"updated_at"`
}

func (BehaviorRollup) TableName() string { return "behavior_rollups" }

// BehaviorBaseline 是当前基线快照（D8 派生表）：每轮从 rollups 全量重算覆盖，
// 不存历史。bucket_key：A 类小时指标为 "0".."23"（hour-of-day），cost 日序列为
// "DAY"，B 类池化分布为 "ALL"。p90 是打分参考分位（D12），p99 仅展示。
type BehaviorBaseline struct {
	ID           int64     `gorm:"primaryKey" json:"id"`
	EntityType   string    `gorm:"size:16;not null;uniqueIndex:idx_behavior_baselines_upsert,priority:1" json:"entity_type"`
	EntityID     int64     `gorm:"not null;uniqueIndex:idx_behavior_baselines_upsert,priority:2" json:"entity_id"`
	Metric       string    `gorm:"size:128;not null;uniqueIndex:idx_behavior_baselines_upsert,priority:3" json:"metric"`
	BucketKey    string    `gorm:"size:8;not null;uniqueIndex:idx_behavior_baselines_upsert,priority:4" json:"bucket_key"`
	P50          float64   `gorm:"not null;default:0" json:"p50"`
	P90          float64   `gorm:"not null;default:0" json:"p90"`
	P99          float64   `gorm:"not null;default:0" json:"p99"`
	EWMA         float64   `gorm:"not null;default:0" json:"ewma"`
	EWMAVar      float64   `gorm:"not null;default:0" json:"ewma_var"`
	ObsCount     int       `gorm:"not null;default:0" json:"obs_count"`
	WindowStart  time.Time `gorm:"not null" json:"window_start"`
	WindowEnd    time.Time `gorm:"not null" json:"window_end"`
	ModelVersion int       `gorm:"not null;default:1" json:"model_version"`
	ComputedAt   time.Time `gorm:"not null" json:"computed_at"`
}

func (BehaviorBaseline) TableName() string { return "behavior_baselines" }

// BehaviorJobLock 是可移植作业互斥（D15）：两步抢锁（UPDATE 过期 → INSERT 撞唯一键）+
// 条件续租/条件释放（owner_token 校验）。fence 单调递增，仅代际记录与诊断，
// 不参与写入校验（明示取舍，规划 §5.3）。
type BehaviorJobLock struct {
	Job        string    `gorm:"primaryKey;size:64" json:"job"`
	OwnerToken string    `gorm:"size:64;not null" json:"owner_token"`
	Fence      int64     `gorm:"not null;default:1" json:"fence"`
	LeaseUntil time.Time `gorm:"not null" json:"lease_until"`
}

func (BehaviorJobLock) TableName() string { return "behavior_job_locks" }

// BehaviorSeenCategory 是新颖度已见集（§5.10）：判定"新类别"的唯一依据（不读 rollups
// 桶存在性），180 天保留，不随 top-32 淘汰。聚合作业每小时 upsert 刷新 last_seen_at。
type BehaviorSeenCategory struct {
	ID          int64     `gorm:"primaryKey" json:"id"`
	EntityType  string    `gorm:"size:16;not null;uniqueIndex:idx_behavior_seen_upsert,priority:1" json:"entity_type"`
	EntityID    int64     `gorm:"not null;uniqueIndex:idx_behavior_seen_upsert,priority:2" json:"entity_id"`
	Kind        string    `gorm:"size:16;not null;uniqueIndex:idx_behavior_seen_upsert,priority:3" json:"kind"` // "model" | "grule"
	Category    string    `gorm:"size:128;not null;uniqueIndex:idx_behavior_seen_upsert,priority:4" json:"category"`
	FirstSeenAt time.Time `gorm:"not null" json:"first_seen_at"`
	LastSeenAt  time.Time `gorm:"not null" json:"last_seen_at"`
}

func (BehaviorSeenCategory) TableName() string { return "behavior_seen_categories" }

// BehaviorAnomaly 是异常事件台账（规划冻结 v4.0 §5.9）。合并语义（D16）在应用层：
// UPDATE 先行锚定 last_hit_at 的滚动 4h 窗、只合并 open；UNIQUE(event_fingerprint,
// merge_bucket) 仅作并发创建兜底。dimensions 内嵌各维度得分与当时基线快照
// （事后可解释），baseline_model_version 记录打分时的模型版本。
type BehaviorAnomaly struct {
	ID               int64     `gorm:"primaryKey" json:"id"`
	EventFingerprint string    `gorm:"size:64;not null;uniqueIndex:idx_behavior_anomalies_dedup,priority:1" json:"event_fingerprint"`
	MergeBucket      int64     `gorm:"not null;uniqueIndex:idx_behavior_anomalies_dedup,priority:2" json:"merge_bucket"`
	EntityType       string    `gorm:"size:16;not null;index:idx_behavior_anomalies_entity,priority:1" json:"entity_type"`
	EntityID         int64     `gorm:"not null;index:idx_behavior_anomalies_entity,priority:2" json:"entity_id"`
	OrgID            *int64    `json:"org_id,omitempty"`
	Score            float64   `gorm:"not null;default:0" json:"score"`
	ScorePeak        float64   `gorm:"not null;default:0" json:"score_peak"`
	Band             string    `gorm:"size:16;not null;default:suspicious" json:"band"`
	Dimensions       *string   `json:"dimensions,omitempty"`
	BaselineModelVer int       `gorm:"column:baseline_model_version;not null;default:1" json:"baseline_model_version"`
	FirstSeenAt      time.Time `gorm:"not null" json:"first_seen_at"`
	LastHitAt        time.Time `gorm:"not null;index:idx_behavior_anomalies_sweep,priority:2" json:"last_hit_at"`
	HitCount         int       `gorm:"not null;default:1" json:"hit_count"`
	WindowStart      time.Time `gorm:"not null" json:"window_start"`
	WindowEnd        time.Time `gorm:"not null" json:"window_end"`
	PrevEventID      *int64    `json:"prev_event_id,omitempty"`
	Status           string    `gorm:"size:16;not null;default:open;index:idx_behavior_anomalies_entity,priority:3;index:idx_behavior_anomalies_sweep,priority:1" json:"status"`
	ClosedReason     *string   `gorm:"size:64" json:"closed_reason,omitempty"`
	DecidedBy        *string   `gorm:"size:64" json:"decided_by,omitempty"`
	CreatedAt        time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt        time.Time `gorm:"not null" json:"updated_at"`
}

func (BehaviorAnomaly) TableName() string { return "behavior_anomalies" }

// BehaviorTolerance 是容忍度乘子（D10/D14）：confirmed/dismissed 反馈的唯一落点，
// t ∈ [0.5, 2.0]，作用于维度得分 s'ᵢ = sᵢ/tᵢⱼ——永不改写 rollups 或基线分位数。
type BehaviorTolerance struct {
	ID             int64     `gorm:"primaryKey" json:"id"`
	EntityType     string    `gorm:"size:16;not null;uniqueIndex:idx_behavior_tolerances_upsert,priority:1" json:"entity_type"`
	EntityID       int64     `gorm:"not null;uniqueIndex:idx_behavior_tolerances_upsert,priority:2" json:"entity_id"`
	Metric         string    `gorm:"size:128;not null;uniqueIndex:idx_behavior_tolerances_upsert,priority:3" json:"metric"`
	Multiplier     float64   `gorm:"not null;default:1" json:"multiplier"`
	LastFeedbackAt time.Time `gorm:"not null" json:"last_feedback_at"`
	CreatedAt      time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt      time.Time `gorm:"not null" json:"updated_at"`
}

func (BehaviorTolerance) TableName() string { return "behavior_tolerances" }

// BehaviorFingerprint 是行为指纹候选库（M3 反馈闭环，§5.6/D10）：confirmed 异常
// 产出候选（命中维度组合 dimension_set + 取值模板 template），人在环复核。
// **审批不自动生效**——observe-only 纪律（D2）：approved 只是把该实体该维度组合
// 记为"已知异常模式"，不触发任何处置；打分层消费是后续里程碑。
type BehaviorFingerprint struct {
	ID            int64     `gorm:"primaryKey" json:"id"`
	EntityType    string    `gorm:"size:16;not null;uniqueIndex:idx_behavior_fps_dedupe,priority:1" json:"entity_type"`
	EntityID      int64     `gorm:"not null;uniqueIndex:idx_behavior_fps_dedupe,priority:2" json:"entity_id"`
	DimensionSet  string    `gorm:"size:256;not null;uniqueIndex:idx_behavior_fps_dedupe,priority:3" json:"dimension_set"` // 命中维度名排序 CSV
	Template      string    `gorm:"not null" json:"template"`                                                              // 各维度取值模板 JSON（x_now/基线/s'）
	SourceEventID *int64    `json:"source_event_id,omitempty"`
	HitCount      int       `gorm:"not null;default:1" json:"hit_count"`
	Status        string    `gorm:"size:16;not null;default:candidate;index:idx_behavior_fps_status" json:"status"` // candidate|approved|retired
	DecidedBy     *string   `gorm:"size:64" json:"decided_by,omitempty"`
	CreatedAt     time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt     time.Time `gorm:"not null" json:"updated_at"`
}

func (BehaviorFingerprint) TableName() string { return "behavior_fingerprints" }
