package repository

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/crosslink/internal/dialect"
	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupAuditSQLiteTestDB creates an in-memory SQLite DB with the full schema applied.
func setupAuditSQLiteTestDB(t *testing.T) (*gorm.DB, dialect.Dialect, func()) {
	t.Helper()

	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)

	db.Exec("PRAGMA foreign_keys = ON")

	// Apply full schema
	sqlBytes, err := os.ReadFile(filepath.Join("migrations", "sqlite", "000001_init_schema.up.sql"))
	require.NoError(t, err)

	// glebarez/sqlite returns TEXT columns as strings which can't scan into time.Time.
	// Replace TEXT typed timestamp columns with DATETIME so the driver parses them.
	schema := strings.ReplaceAll(string(sqlBytes), "TEXT NOT NULL DEFAULT (datetime('now'))", "DATETIME NOT NULL DEFAULT (datetime('now'))")

	for _, stmt := range splitAuditSQL(schema) {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		require.NoError(t, db.Exec(stmt).Error, "schema statement failed: %s", truncateAudit(stmt, 100))
	}

	dia := dialect.NewSQLiteDialect(dialect.DBConfig{Driver: "sqlite"})
	cleanup := func() {
		sqlDB, _ := db.DB()
		sqlDB.Close()
	}
	return db, dia, cleanup
}

func TestAuditLogRepo_ILike_SQLite(t *testing.T) {
	db, dia, cleanup := setupAuditSQLiteTestDB(t)
	defer cleanup()

	repo := NewAuditLogRepo(db, dia)

	// Insert audit logs with mixed-case values
	now := time.Now()
	rows := []*model.AuditLog{
		{UserID: 1, Username: "TestUser", Action: "create", ResourceType: "key", ResourceID: "1", ResourceName: "TestResource", Status: "success", CreatedAt: now},
		{UserID: 2, Username: "admin", Action: "delete", ResourceType: "key", ResourceID: "2", ResourceName: "Production Key", Status: "success", CreatedAt: now},
		{UserID: 3, Username: "viewer", Action: "list", ResourceType: "key", ResourceID: "3", ResourceName: "OLD_KEY", Status: "success", CreatedAt: now},
	}
	require.NoError(t, repo.CreateBatch(context.Background(), rows))

	// Search for "test" — should match TestUser and TestResource (case-insensitive)
	logs, total, err := repo.List(context.Background(), AuditFilter{Q: "test"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total, "ILike search for 'test' should match 1 row")
	if len(logs) > 0 {
		assert.Equal(t, "TestUser", logs[0].Username)
	}

	// Search for "key" — should match Production Key and OLD_KEY
	_, total2, err := repo.List(context.Background(), AuditFilter{Q: "key"})
	require.NoError(t, err)
	assert.Equal(t, int64(2), total2, "ILike search for 'key' should match 2 rows")

	// Search for "OLD" — should match OLD_KEY
	logs3, total3, err := repo.List(context.Background(), AuditFilter{Q: "OLD"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total3, "ILike search for 'OLD' should match 1 row")
	if len(logs3) > 0 {
		assert.Equal(t, "OLD_KEY", logs3[0].ResourceName)
	}
}

func TestAuditLogRepo_DateRange_SQLite(t *testing.T) {
	db, dia, cleanup := setupAuditSQLiteTestDB(t)
	defer cleanup()

	repo := NewAuditLogRepo(db, dia)

	now := time.Now()
	rows := []*model.AuditLog{
		{UserID: 1, Username: "u1", Action: "create", ResourceType: "key", ResourceID: "1", ResourceName: "early", Status: "success", CreatedAt: now.AddDate(0, 0, -30)},
		{UserID: 2, Username: "u2", Action: "create", ResourceType: "key", ResourceID: "2", ResourceName: "mid", Status: "success", CreatedAt: now.AddDate(0, 0, -10)},
		{UserID: 3, Username: "u3", Action: "create", ResourceType: "key", ResourceID: "3", ResourceName: "recent", Status: "success", CreatedAt: now.AddDate(0, 0, -1)},
	}
	require.NoError(t, repo.CreateBatch(context.Background(), rows))

	// Only mid-range row (within last 15 days)
	startDate := now.AddDate(0, 0, -15).Format("2006-01-02")
	endDate := now.AddDate(0, 0, -5).Format("2006-01-02")
	logs, total, err := repo.List(context.Background(), AuditFilter{StartDate: startDate, EndDate: endDate})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total, "date range should match 1 row")
	if len(logs) > 0 {
		assert.Equal(t, "mid", logs[0].ResourceName)
	}
}

func TestAuditLogRepo_DeleteBefore_SQLite(t *testing.T) {
	db, dia, cleanup := setupAuditSQLiteTestDB(t)
	defer cleanup()

	repo := NewAuditLogRepo(db, dia)

	now := time.Now()
	var rows []*model.AuditLog
	// Insert 5 old rows (30 days ago)
	for i := 0; i < 5; i++ {
		rows = append(rows, &model.AuditLog{
			UserID: 1, Username: "u", Action: "delete", ResourceType: "log", ResourceID: fmt.Sprintf("%d", i),
			ResourceName: "old", Status: "success", CreatedAt: now.AddDate(0, 0, -30),
		})
	}
	// Insert 2 recent rows
	for i := 0; i < 2; i++ {
		rows = append(rows, &model.AuditLog{
			UserID: 1, Username: "u", Action: "create", ResourceType: "log", ResourceID: fmt.Sprintf("r%d", i),
			ResourceName: "recent", Status: "success", CreatedAt: now,
		})
	}
	require.NoError(t, repo.CreateBatch(context.Background(), rows))

	// Delete rows older than 10 days
	cutoff := now.AddDate(0, 0, -10)
	deleted, err := repo.DeleteBefore(context.Background(), cutoff)
	require.NoError(t, err)
	assert.Equal(t, int64(5), deleted, "should delete 5 old rows")

	// Verify 2 recent rows remain
	var remaining int64
	db.Model(&model.AuditLog{}).Count(&remaining)
	assert.Equal(t, int64(2), remaining, "2 recent rows should remain")
}

// splitAuditSQL splits a SQL file into individual statements.
// Skips empty lines and comments (-- ...).
func splitAuditSQL(sql string) []string {
	var stmts []string
	var current strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		current.WriteString(line)
		current.WriteString("\n")
		if strings.HasSuffix(trimmed, ";") {
			s := strings.TrimSpace(current.String())
			if s != "" {
				stmts = append(stmts, s)
			}
			current.Reset()
		}
	}
	if current.Len() > 0 {
		s := strings.TrimSpace(current.String())
		if s != "" {
			stmts = append(stmts, s)
		}
	}
	return stmts
}

// truncateAudit shortens a string for error messages.
func truncateAudit(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
