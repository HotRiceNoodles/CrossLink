package model

import (
	"time"

	"gorm.io/datatypes"
)

// GuardIngestToken 旁路检测接入凭证（crosslink-guard 专用，设计 DD-7）。
// 不复用 api_keys——模型权限/预算/IP 绑定等网关语义对旁路接入过重；
// 哈希存储与轮换模式借用 api_key_hashes。明文 token 仅创建时展示一次，
// 库中只存 SHA-256 哈希与可展示前缀。
type GuardIngestToken struct {
	ID         int64      `gorm:"primaryKey" json:"id"`
	OrgID      int64      `gorm:"not null;index:idx_guard_ingest_tokens_org" json:"org_id"`
	Name       string     `gorm:"size:128;not null;default:''" json:"name"`
	TokenHash  string     `gorm:"size:64;uniqueIndex:uq_guard_ingest_tokens_hash;not null" json:"-"`
	KeyPrefix  string     `gorm:"size:16;not null;default:''" json:"key_prefix"`
	HashAlgo   string     `gorm:"size:10;not null;default:'sha256'" json:"hash_algo"`
	Status     int16      `gorm:"not null;default:1" json:"status"` // 1=active 0=revoked
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	CreatedAt  time.Time  `gorm:"not null" json:"created_at"`
}

func (GuardIngestToken) TableName() string { return "guard_ingest_tokens" }

// GuardEvent 旁路检测事件主留存（设计 DD-5）。
//
// 脱敏约束（红线，compliance_incidents 同模式）：
//   - ContentPreview 只存 SanitizePreview 脱敏摘要，绝不保存敏感原文；
//   - ContentSHA256 保存原文哈希用于关联与去重，不可逆。
//
// Action 取值：would_block（旁路影子判定，block 的旁路语义）、review、
// log、mask——旁路不产生真实的 block。
type GuardEvent struct {
	ID             int64          `gorm:"primaryKey" json:"id"`
	TraceID        string         `gorm:"size:64;not null;default:'';index:idx_guard_events_trace" json:"trace_id"`
	OrgID          int64          `gorm:"not null;default:0;index:idx_guard_events_org" json:"org_id"`
	RuleID         int64          `gorm:"not null;default:0" json:"rule_id"`
	RuleVersion    int64          `gorm:"not null;default:0" json:"rule_version"`
	EngineType     string         `gorm:"size:50;not null;default:''" json:"engine_type"`
	Direction      string         `gorm:"size:10;not null" json:"direction"` // request|response
	Action         string         `gorm:"size:20;not null" json:"action"`
	Severity       string         `gorm:"size:20;not null;default:''" json:"severity"`
	Reason         string         `gorm:"size:1000;not null;default:''" json:"reason"`
	ContentPreview string         `gorm:"size:500;not null;default:''" json:"content_preview"`
	ContentSHA256  string         `gorm:"size:64;not null;default:''" json:"content_sha256"`
	Model          string         `gorm:"size:255;not null;default:''" json:"model"`
	Metadata       datatypes.JSON `gorm:"not null" json:"metadata"` // 客户 payload metadata 透传（租户内部门细分靠它过滤）
	CreatedAt      time.Time      `gorm:"not null" json:"created_at"`
}

func (GuardEvent) TableName() string { return "guard_events" }

// GuardTrace 生命周期状态。
const (
	GuardTraceOpen     = "open"
	GuardTraceComplete = "complete"
	GuardTraceExpired  = "expired" // 预留给留存/过期扫描标记
)

// GuardTrace 裁决汇总档位（高档覆盖低档；error 只覆盖 pending）。
const (
	GuardVerdictPending    = "pending"
	GuardVerdictPass       = "pass"
	GuardVerdictFlagged    = "flagged"
	GuardVerdictWouldBlock = "would_block"
	GuardVerdictError      = "error"
)

// GuardTrace 旁路检测 trace 元数据（设计 DD-6）：状态、方向完成时间、
// 裁决汇总——无任何内容列，原文只存在于内存与 TTL 配对窗口。
type GuardTrace struct {
	ID         int64      `gorm:"primaryKey" json:"id"`
	TraceID    string     `gorm:"size:64;not null;uniqueIndex:uq_guard_traces_org_trace" json:"trace_id"`
	OrgID      int64      `gorm:"not null;uniqueIndex:uq_guard_traces_org_trace;index:idx_guard_traces_org_created" json:"org_id"`
	Status     string     `gorm:"size:20;not null;default:'open'" json:"status"`     // GuardTrace* 常量
	Verdict    string     `gorm:"size:20;not null;default:'pending'" json:"verdict"` // GuardVerdict* 常量
	RequestAt  *time.Time `json:"request_at"`                                        // 请求向检测完成时间（NULL=未完成）
	ResponseAt *time.Time `json:"response_at"`                                       // 响应向检测完成时间（NULL=未完成）
	CreatedAt  time.Time  `gorm:"not null" json:"created_at"`
	UpdatedAt  time.Time  `gorm:"not null" json:"updated_at"`
}

func (GuardTrace) TableName() string { return "guard_traces" }
