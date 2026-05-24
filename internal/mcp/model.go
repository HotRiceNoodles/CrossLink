package mcp

import (
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// MCPServer stores registered MCP server configurations.
type MCPServer struct {
	ID              int64          `gorm:"primaryKey" json:"id"`
	Name            string         `gorm:"uniqueIndex;size:64;not null" json:"name"`
	DisplayName     string         `gorm:"size:128" json:"display_name"`
	Description     string         `gorm:"size:512" json:"description"`
	TransportType   string         `gorm:"size:16;not null" json:"transport_type"` // http | sse | stdio
	URL             string         `gorm:"size:512" json:"url"`
	StdioConfig     datatypes.JSON `gorm:"type:jsonb" json:"stdio_config"`
	AuthType        string         `gorm:"size:32;default:none" json:"auth_type"` // none | bearer | basic | oauth2 | sigv4
	AuthConfig      datatypes.JSON `gorm:"type:jsonb" json:"auth_config"`
	CustomHeaders   datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"custom_headers"`
	Status          int16          `gorm:"not null;default:1" json:"status"`        // 1=active, 0=inactive, -1=error
	HealthStatus    int16          `gorm:"not null;default:0" json:"health_status"` // 1=healthy, 0=unknown, -1=unhealthy
	LastHealthCheck *time.Time     `json:"last_health_check"`
	ToolCount       int            `gorm:"default:0" json:"tool_count"`
	Enabled         bool           `gorm:"default:true" json:"enabled"`
	TierRequired    string         `gorm:"size:16;default:community" json:"tier_required"`
	CreatedBy       int64          `json:"created_by"`
	CreatedAt       time.Time      `gorm:"not null;default:now()" json:"created_at"`
	UpdatedAt       time.Time      `gorm:"not null;default:now()" json:"updated_at"`
	DeletedAt       gorm.DeletedAt `json:"-" gorm:"index"`
}

func (MCPServer) TableName() string { return "mcp_servers" }

// MCPServerPermission controls per-principal access to MCP servers and tools.
type MCPServerPermission struct {
	ID            int64          `gorm:"primaryKey" json:"id"`
	ServerID      int64          `gorm:"index;not null" json:"server_id"`
	PrincipalType string         `gorm:"size:16;not null" json:"principal_type"` // key | team | role
	PrincipalID   int64          `gorm:"not null" json:"principal_id"`
	AllowTools    datatypes.JSON `gorm:"type:jsonb;default:'[\"*\"]'" json:"allow_tools"`
	DenyTools     datatypes.JSON `gorm:"type:jsonb;default:'[]'" json:"deny_tools"`
	CreatedAt     time.Time      `gorm:"not null;default:now()" json:"created_at"`
}

func (MCPServerPermission) TableName() string { return "mcp_server_permissions" }

// MCPToolCallLog records MCP tool invocations.
// Uses PostgreSQL monthly partitioning; no gorm:"primaryKey" tag.
type MCPToolCallLog struct {
	ID         int64     `json:"id"`
	RequestID  string    `gorm:"index;size:36" json:"request_id"`
	ServerID   int64     `gorm:"index" json:"server_id"`
	ServerName string    `gorm:"size:64" json:"server_name"`
	ToolName   string    `gorm:"size:128" json:"tool_name"`
	Method     string    `gorm:"size:32" json:"method"`
	InputSize  int       `gorm:"default:0" json:"input_size"`
	OutputSize int       `gorm:"default:0" json:"output_size"`
	Duration   int       `gorm:"default:0" json:"duration"`
	Status     int16     `gorm:"not null" json:"status"` // 1=success, 0=error, -1=blocked
	ErrorCode  int       `json:"error_code"`
	ErrorMsg   string    `gorm:"size:512" json:"error_msg"`
	APIKeyID   int64     `gorm:"index" json:"api_key_id"`
	UserID     int64     `json:"user_id"`
	TeamID     int64     `gorm:"index" json:"team_id"`
	BlockedBy  string    `gorm:"size:64" json:"blocked_by"`
	CreatedAt  time.Time `gorm:"not null;default:now();index" json:"created_at"`
}

func (MCPToolCallLog) TableName() string { return "mcp_tool_call_logs" }

// MCPToolCallStats holds aggregated metrics for tool calls.
type MCPToolCallStats struct {
	TotalCalls   int64   `json:"total_calls"`
	SuccessCount int64   `json:"success_count"`
	ErrorCount   int64   `json:"error_count"`
	BlockedCount int64   `json:"blocked_count"`
	AvgDuration  float64 `json:"avg_duration_ms"`
	P95Duration  float64 `json:"p95_duration_ms"`
	TotalInput   int64   `json:"total_input_bytes"`
	TotalOutput  int64   `json:"total_output_bytes"`
}

// MCPTopTool is a per-tool usage breakdown.
type MCPTopTool struct {
	Name      string  `json:"name"`
	Count     int64   `json:"count"`
	AvgDur    float64 `json:"avg_duration_ms"`
	ErrorRate float64 `json:"error_rate"`
}

// MCPDailyCalls is a daily aggregation of tool call counts.
type MCPDailyCalls struct {
	Date    string `json:"date"`
	Count   int64  `json:"count"`
	Success int64  `json:"success"`
	Error   int64  `json:"error"`
}
