package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/crosslink/internal/dialect"
	"gorm.io/gorm"
)

type MCPRepo struct {
	db  *gorm.DB
	dia dialect.Dialect
}

func NewMCPRepo(db *gorm.DB, dia dialect.Dialect) *MCPRepo {
	return &MCPRepo{db: db, dia: dia}
}

func (r *MCPRepo) baseQuery(orgID int64) *gorm.DB {
	q := r.db.Model(&MCPServer{})
	if orgID != 0 {
		q = q.Where("org_id = ?", orgID)
	}
	return q
}

func (r *MCPRepo) Create(ctx context.Context, srv *MCPServer) error {
	return r.db.WithContext(ctx).Create(srv).Error
}

// CreateWithLimit atomically checks server count and creates within a transaction.
func (r *MCPRepo) CreateWithLimit(ctx context.Context, srv *MCPServer, maxServers int) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if maxServers > 0 {
			var count int64
			if err := tx.Model(&MCPServer{}).Count(&count).Error; err != nil {
				return err
			}
			if count >= int64(maxServers) {
				return fmt.Errorf("community edition allows at most %d MCP servers", maxServers)
			}
		}
		return tx.Create(srv).Error
	})
}

func (r *MCPRepo) GetByID(ctx context.Context, orgID int64, id int64) (*MCPServer, error) {
	var srv MCPServer
	q := r.db.WithContext(ctx)
	if orgID != 0 {
		q = q.Where("org_id = ?", orgID)
	}
	if err := q.First(&srv, id).Error; err != nil {
		return nil, err
	}
	return &srv, nil
}

func (r *MCPRepo) GetByName(ctx context.Context, orgID int64, name string) (*MCPServer, error) {
	var srv MCPServer
	q := r.db.WithContext(ctx).Where("name = ?", name)
	if orgID != 0 {
		q = q.Where("org_id = ?", orgID)
	}
	if err := q.First(&srv).Error; err != nil {
		return nil, err
	}
	return &srv, nil
}

func (r *MCPRepo) List(ctx context.Context, orgID int64) ([]MCPServer, error) {
	var servers []MCPServer
	if err := r.baseQuery(orgID).WithContext(ctx).Limit(500).Find(&servers).Error; err != nil {
		return nil, err
	}
	return servers, nil
}

func (r *MCPRepo) Update(ctx context.Context, srv *MCPServer) error {
	return r.db.WithContext(ctx).Select(
		"display_name", "description", "url", "enabled",
		"transport_type", "auth_type", "auth_config", "custom_headers",
	).Save(srv).Error
}

func (r *MCPRepo) Delete(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&MCPServer{}, id).Error
}

func (r *MCPRepo) CountActive(ctx context.Context, orgID int64) (int64, error) {
	var count int64
	if err := r.baseQuery(orgID).WithContext(ctx).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func (r *MCPRepo) LogToolCall(ctx context.Context, log *MCPToolCallLog) error {
	return r.db.WithContext(ctx).Create(log).Error
}

func (r *MCPRepo) UpdateHealthStatus(ctx context.Context, id int64, status int16) error {
	now := time.Now()
	return r.db.WithContext(ctx).Model(&MCPServer{}).
		Where("id = ?", id).
		Updates(map[string]interface{}{
			"health_status":     status,
			"last_health_check": now,
		}).Error
}

func (r *MCPRepo) UpdateToolCount(ctx context.Context, id int64, count int) error {
	return r.db.WithContext(ctx).Model(&MCPServer{}).
		Where("id = ?", id).
		Update("tool_count", count).Error
}

func (r *MCPRepo) DeleteLogsBefore(ctx context.Context, before time.Time) error {
	const batchSize = 5000
	for {
		result := r.db.WithContext(ctx).
			Where("created_at < ?", before).
			Limit(batchSize).
			Delete(&MCPToolCallLog{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
	}
}

func (r *MCPRepo) ListPermissions(ctx context.Context, serverID int64) ([]MCPServerPermission, error) {
	var perms []MCPServerPermission
	if err := r.db.WithContext(ctx).Where("server_id = ?", serverID).Find(&perms).Error; err != nil {
		return nil, err
	}
	return perms, nil
}

func (r *MCPRepo) CreatePermission(ctx context.Context, perm *MCPServerPermission) error {
	return r.db.WithContext(ctx).Create(perm).Error
}

func (r *MCPRepo) DeletePermission(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&MCPServerPermission{}, id).Error
}

func (r *MCPRepo) ListToolCallLogs(ctx context.Context, orgID int64, serverID int64, page, pageSize int) ([]MCPToolCallLog, int64, error) {
	var total int64
	q := r.db.WithContext(ctx).Model(&MCPToolCallLog{})
	if orgID != 0 {
		q = q.Where("org_id = ?", orgID)
	}
	if serverID > 0 {
		q = q.Where("server_id = ?", serverID)
	}
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var logs []MCPToolCallLog
	offset := (page - 1) * pageSize
	if err := q.Order("created_at DESC").Offset(offset).Limit(pageSize).Find(&logs).Error; err != nil {
		return nil, 0, err
	}
	return logs, total, nil
}

func (r *MCPRepo) GetToolCallStats(ctx context.Context, orgID int64, serverID int64, days int) (*MCPToolCallStats, error) {
	since := time.Now().AddDate(0, 0, -days)
	stats := &MCPToolCallStats{}
	q := r.db.WithContext(ctx).Model(&MCPToolCallLog{}).Where("created_at >= ?", since)
	if orgID != 0 {
		q = q.Where("org_id = ?", orgID)
	}
	if serverID > 0 {
		q = q.Where("server_id = ?", serverID)
	}
	exprs := []any{
		"COUNT(*)",
		r.dia.ConditionalCount("status", "1"),
		r.dia.ConditionalCount("status", "0"),
		r.dia.ConditionalCount("status", "-1"),
		"COALESCE(AVG(duration), 0)",
		"COALESCE(SUM(input_size), 0)",
		"COALESCE(SUM(output_size), 0)",
	}
	if r.dia.Name() == "postgres" || r.dia.Name() == "kingbase" {
		exprs = append(exprs, "COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY duration), 0)")
	} else {
		exprs = append(exprs, "0")
	}
	row := q.Select(exprs[0], exprs[1:]...).Row()
	if err := row.Scan(&stats.TotalCalls, &stats.SuccessCount, &stats.ErrorCount,
		&stats.BlockedCount, &stats.AvgDuration, &stats.TotalInput,
		&stats.TotalOutput, &stats.P95Duration); err != nil {
		return nil, err
	}
	return stats, nil
}

func (r *MCPRepo) GetTopTools(ctx context.Context, orgID int64, serverID int64, days int, limit int) ([]MCPTopTool, error) {
	since := time.Now().AddDate(0, 0, -days)
	errorCountExpr := r.dia.ConditionalCount("status", "0") + " + " + r.dia.ConditionalCount("status", "-1")
	q := r.db.WithContext(ctx).Model(&MCPToolCallLog{}).
		Select("tool_name as name, COUNT(*) as count, COALESCE(AVG(duration),0) as avg_dur, COALESCE(" + r.dia.CastFloat(errorCountExpr) + " / NULLIF(COUNT(*), 0), 0) as error_rate").
		Where("created_at >= ? AND tool_name != ''", since).
		Group("tool_name").Order("count DESC")
	if orgID != 0 {
		q = q.Where("org_id = ?", orgID)
	}
	if serverID > 0 {
		q = q.Where("server_id = ?", serverID)
	}
	if limit > 0 {
		q = q.Limit(limit)
	}
	var tools []MCPTopTool
	if err := q.Scan(&tools).Error; err != nil {
		return nil, err
	}
	return tools, nil
}

func (r *MCPRepo) GetCallsByDay(ctx context.Context, orgID int64, serverID int64, days int) ([]MCPDailyCalls, error) {
	since := time.Now().AddDate(0, 0, -days)
	successExpr := r.dia.ConditionalCount("status", "1")
	errorExpr := r.dia.ConditionalCount("status", "0") + " + " + r.dia.ConditionalCount("status", "-1")
	q := r.db.WithContext(ctx).Model(&MCPToolCallLog{}).
		Select("DATE(created_at) as date, COUNT(*) as count, " + successExpr + " as success, " + errorExpr + " as error").
		Where("created_at >= ?", since).
		Group("DATE(created_at)").Order("date ASC")
	if orgID != 0 {
		q = q.Where("org_id = ?", orgID)
	}
	if serverID > 0 {
		q = q.Where("server_id = ?", serverID)
	}
	var result []MCPDailyCalls
	if err := q.Scan(&result).Error; err != nil {
		return nil, err
	}
	return result, nil
}
