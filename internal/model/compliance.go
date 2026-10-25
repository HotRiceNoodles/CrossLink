package model

import (
	"time"

	"gorm.io/datatypes"
)

// ComplianceIncident 内容合规事件（商业版 content_compliance 引擎命中记录）。
//
// 脱敏约束（红线）：
//   - ContentExcerpt 只存引擎脱敏后的证据摘录（命中词首字符+***、且经
//     SanitizePreview 二次脱敏），绝不保存完整敏感原文；
//   - ContentSHA256 保存原文哈希用于关联与去重，不可逆；
//   - Categories 内的 evidence/masked_term 均为脱敏值。
//
// 复核诚实性约束：status=pending_review 只能由真人通过复核 API 变更为
// confirmed/false_positive，同时落 ReviewerID/ReviewerName/ReviewedAt；
// 不存在任何把待复核自动置为已复核的代码路径。
type ComplianceIncident struct {
	ID            int64  `gorm:"primaryKey" json:"id"`
	OrgID         *int64 `gorm:"index:idx_compliance_incidents_org" json:"org_id,omitempty"`
	RuleID        int64  `gorm:"not null;default:0" json:"rule_id"`
	RuleVersion   int64  `gorm:"not null;default:0" json:"rule_version"`
	Direction     string `gorm:"size:10;not null" json:"direction"`    // request|response
	ActionTaken   string `gorm:"size:20;not null" json:"action_taken"` // block|review|log|pass
	Status        string `gorm:"size:24;not null;index" json:"status"` // pending_review|auto_blocked|logged|confirmed|false_positive
	Categories    datatypes.JSON `json:"categories"`                    // []ComplianceLabel（脱敏）
	MaxRiskLevel  string `gorm:"size:20;not null;default:''" json:"max_risk_level"`
	Reason        string `gorm:"size:1000" json:"reason"`
	ContentSHA256 string `gorm:"size:64;not null;default:''" json:"content_sha256"`
	ContentExcerpt string `gorm:"size:500" json:"content_excerpt"`
	EngineError   string `gorm:"size:500" json:"engine_error,omitempty"`
	Model         string `gorm:"size:255" json:"model"`
	APIKeyID      int64  `json:"api_key_id"`
	TeamID        int64  `json:"team_id"`
	ReviewerID    int64  `gorm:"not null;default:0" json:"reviewer_id"`
	ReviewerName  string `gorm:"size:64;not null;default:''" json:"reviewer_name"`
	ReviewNote    string `gorm:"size:500;not null;default:''" json:"review_note"`
	ReviewedAt    *time.Time `json:"reviewed_at"`
	CreatedAt     time.Time  `gorm:"not null;default:now()" json:"created_at"`
}

func (ComplianceIncident) TableName() string { return "compliance_incidents" }
