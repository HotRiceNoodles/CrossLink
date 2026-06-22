package model

import (
	"time"

	"gorm.io/gorm"
)

const (
	RoleAdmin    = "admin"
	RoleMember   = "member"
	RoleViewer   = "viewer"
	RoleOrgAdmin = "org_admin"
)

// ValidActions is the canonical set of permission actions.
var ValidActions = map[string]bool{
	"provider:list":               true,
	"provider:create":             true,
	"provider:update":             true,
	"provider:delete":             true,
	"provider:test":               true,
	"model:list":                  true,
	"model:create":                true,
	"model:update":                true,
	"model:delete":                true,
	"key:list":                    true,
	"key:create":                  true,
	"key:update":                  true,
	"key:delete":                  true,
	"key:regenerate":              true,
	"key:rotate":                  true,
	"key:hashes":                  true,
	"team:list":                   true,
	"team:create":                 true,
	"team:update":                 true,
	"team:delete":                 true,
	"team:manage_members":         true,
	"user:list":                   true,
	"user:create":                 true,
	"user:update":                 true,
	"user:delete":                 true,
	"usage:list":                  true,
	"usage:export":                true,
	"usage:stats":                 true,
	"system:view":                 true,
	"system:update":               true,
	"debug:list":                  true,
	"debug:clear":                 true,
	"budget:manage":               true,
	"guardrail:list":              true,
	"guardrail:create":            true,
	"guardrail:update":            true,
	"guardrail:delete":            true,
	"guardrail:test":              true,
	"secret:test":                 true,
	"secret:manage":               true,
	"audit:list":                  true,
	"audit:export":                true,
	"playground:use":              true,
	"insight:manage":              true,
	"guardrail_alert:list":        true,
	"guardrail_alert:create":      true,
	"guardrail_alert:update":      true,
	"guardrail_alert:delete":      true,
	"guardrail_alert:logs":        true,
	"system:password":             true,
	"license:view":                true,
	"license:manage":              true,
	"agent_shield:view":           true,
	"agent_shield:manage":         true,
	"fingerprint:list":            true,
	"fingerprint:view":            true,
	"fingerprint:manage":          true,
	"mcp:list":                    true,
	"mcp:view":                    true,
	"mcp:create":                  true,
	"mcp:update":                  true,
	"mcp:delete":                  true,
	"mcp:permission":              true,
	"mcp:logs":                    true,
	"mcp:stats":                   true,
	"org:list":                    true,
	"org:create":                  true,
	"org:update":                  true,
	"org:delete":                  true,
	"org:manage_members":          true,
	"org:view_billing":            true,
	"org:manage_billing":          true,
	"role:list":                   true,
	"role:create":                 true,
	"role:update":                 true,
	"role:delete":                 true,
	"datalens:view":               true,
	"datalens:create":             true,
	"datalens:update":             true,
	"datalens:delete":             true,
	"datalens:export":             true,
	"datalens:schedule":           true,
	"datalens:manage_aggregation": true,
	"error_rule:list":             true,
	"error_rule:create":           true,
	"error_rule:update":           true,
	"error_rule:delete":           true,
	"capability:list":             true,
	"capability:create":           true,
	"capability:update":           true,
	"capability:delete":           true,
}

// AdminRequiredActions are actions that must always remain on the admin role.
var AdminRequiredActions = map[string]bool{
	"user:list":     true,
	"user:create":   true,
	"user:update":   true,
	"user:delete":   true,
	"system:view":   true,
	"system:update": true,
}

// AdminExclusiveActions are actions that ONLY the admin (super admin) role can have.
// Unlike AdminRequiredActions (which also includes user:* for org_admin),
// these represent system-level control that must never be delegated.
var AdminExclusiveActions = map[string]bool{
	"system:view":       true,
	"system:update":     true,
	"error_rule:list":   true,
	"error_rule:create": true,
	"error_rule:update": true,
	"error_rule:delete": true,
}

func IsValidAction(action string) bool {
	return ValidActions[action]
}

type Role struct {
	ID          int64          `gorm:"primaryKey" json:"id"`
	Name        string         `gorm:"size:32;uniqueIndex;not null" json:"name"`
	DisplayName string         `gorm:"size:64;not null" json:"display_name"`
	IsSystem    bool           `gorm:"not null;default:false" json:"is_system"`
	OrgID       *int64         `gorm:"index" json:"org_id"`
	CreatedAt   time.Time      `gorm:"not null" json:"created_at"`
	DeletedAt   gorm.DeletedAt `json:"-" gorm:"index"`
}

func (Role) TableName() string { return "roles" }

type RolePermission struct {
	ID     int64  `gorm:"primaryKey" json:"id"`
	RoleID int64  `gorm:"not null;index" json:"role_id"`
	Action string `gorm:"size:64;not null" json:"action"`
}

func (RolePermission) TableName() string { return "role_permissions" }
