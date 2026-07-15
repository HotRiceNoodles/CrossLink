package model

import (
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// PromptTemplate is a composable context block set: the gateway assembles its
// system_prompt + few_shot into requests that reference it by name via the
// `x_context` field. See docs/plans/2026-07-14-context-engineering-gateway-design.md.
//
// MVP stores a flat template (system/few-shot/tools as separate fields) so v2 can
// split them into reusable context_blocks without breaking this table.
//
// NOTE: name uniqueness is enforced by a PARTIAL unique index
// (prompt_templates_name_key WHERE deleted_at IS NULL) created in migration 000068,
// NOT by a gorm uniqueIndex tag — soft-deleted rows must not block re-creation
// (same pattern as providers, migration 000067). Do not add uniqueIndex here.
type PromptTemplate struct {
	ID              int64          `gorm:"primaryKey" json:"id"`
	Name            string         `gorm:"size:64;not null" json:"name"`
	Description     string         `gorm:"size:512" json:"description,omitempty"`
	SystemPrompt    string         `gorm:"type:text;not null;default:''" json:"system_prompt"`
	VariablesSchema datatypes.JSON `json:"variables_schema,omitempty"` // [{name,type,required,default,trusted,desc}]
	FewShot         datatypes.JSON `json:"few_shot,omitempty"`         // [{role,content}] static
	ToolDefs        datatypes.JSON `json:"tool_defs,omitempty"`        // reserved (MVP empty)
	TargetFormat    string         `gorm:"size:16;not null;default:'auto'" json:"target_format"` // auto|anthropic|openai
	Status          int16          `gorm:"not null;default:1" json:"status"`                       // 1=active, 0=disabled
	Version         int            `gorm:"not null;default:1" json:"-"` // MVP fixed 1; hidden to avoid implying versioning
	OrgID           *int64         `gorm:"index" json:"org_id,omitempty"`
	CreatedAt       time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt       time.Time      `gorm:"not null" json:"updated_at"`
	DeletedAt       gorm.DeletedAt `json:"-" gorm:"index"`
}

func (PromptTemplate) TableName() string { return "prompt_templates" }
