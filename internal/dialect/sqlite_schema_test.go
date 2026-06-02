package dialect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestSQLiteSchema_Up validates that the full SQLite schema file loads
// without errors into an in-memory SQLite database.
func TestSQLiteSchema_Up(t *testing.T) {
	sqlBytes, err := os.ReadFile(filepath.Join("..", "..", "migrations", "sqlite", "000001_init_schema.up.sql"))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}

	// Enable foreign keys for the in-memory connection
	db.Exec("PRAGMA foreign_keys = ON")

	// Execute each statement
	statements := splitSchemaStatements(string(sqlBytes))
	for i, stmt := range statements {
		if stmt == "" {
			continue
		}
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("statement %d: %v\n%s", i+1, err, truncStr(stmt, 200))
		}
	}

	// Verify all expected tables exist
	tables := []string{
		"system_settings",
		"roles",
		"users",
		"organizations",
		"organization_members",
		"teams",
		"team_members",
		"role_permissions",
		"providers",
		"provider_models",
		"api_keys",
		"api_key_hashes",
		"usage_logs",
		"guardrail_rules",
		"guardrail_alert_rules",
		"guardrail_alert_logs",
		"budget_alerts",
		"budget_snapshots",
		"budget_recommendations",
		"budget_requests",
		"audit_logs",
		"insights",
		"optimization_actions",
		"agent_fingerprints",
		"mcp_servers",
		"mcp_server_permissions",
		"mcp_tool_call_logs",
		"mcp_tool_call_logs_archive",
	}
	for _, table := range tables {
		var count int64
		db.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&count)
		if count != 1 {
			t.Errorf("table %q not found in sqlite_master", table)
		}
	}

	// Verify key indexes exist
	indexes := []string{
		"idx_api_key_hashes_key_hash",
		"idx_api_key_hashes_one_primary",
		"idx_usage_logs_created_at",
		"idx_usage_logs_fallback",
		"idx_usage_logs_cache_hit",
		"provider_models_active_unique",
		"organizations_name_active_idx",
		"idx_agent_fingerprints_dedup",
		"idx_insights_unique",
		"idx_mcp_perm_principal",
	}
	for _, idx := range indexes {
		var count int64
		db.Raw("SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?", idx).Scan(&count)
		if count != 1 {
			t.Errorf("index %q not found in sqlite_master", idx)
		}
	}

	t.Logf("OK: %d tables and %d key indexes verified", len(tables), len(indexes))
}

// TestSQLiteSchema_Down validates that the down migration drops all tables cleanly.
func TestSQLiteSchema_Down(t *testing.T) {
	// First, apply the up migration
	sqlUp, err := os.ReadFile(filepath.Join("..", "..", "migrations", "sqlite", "000001_init_schema.up.sql"))
	if err != nil {
		t.Fatalf("read up schema: %v", err)
	}

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}

	db.Exec("PRAGMA foreign_keys = ON")

	for _, stmt := range splitSchemaStatements(string(sqlUp)) {
		if stmt == "" {
			continue
		}
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("up statement: %v", err)
		}
	}

	// Now apply the down migration
	sqlDown, err := os.ReadFile(filepath.Join("..", "..", "migrations", "sqlite", "000001_init_schema.down.sql"))
	if err != nil {
		t.Fatalf("read down schema: %v", err)
	}

	for _, stmt := range splitSchemaStatements(string(sqlDown)) {
		if stmt == "" {
			continue
		}
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("down statement: %v\n%s", err, truncStr(stmt, 200))
		}
	}

	// Verify no user tables remain (sqlite_master should only have auto-created internals)
	var count int64
	db.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&count)
	if count != 0 {
		var names []string
		db.Raw("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&names)
		t.Fatalf("expected 0 tables after down migration, got %d: %v", count, names)
	}

	t.Log("OK: down migration drops all tables cleanly")
}

// TestSQLiteDialect_RunMigrations tests the full RunMigrations flow:
// InitDB → RunMigrations → verify schema_migrations table and key tables.
func TestSQLiteDialect_RunMigrations(t *testing.T) {
	// RunMigrations reads from migrations/sqlite/ which is relative to project root.
	// The test process cwd is internal/dialect/, so we need to change to project root.
	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)

	dir, err := os.MkdirTemp("", "crosslink-sqlite-migration-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	dbPath := filepath.Join(dir, "test.db")
	d := NewSQLiteDialect(DBConfig{Driver: "sqlite", SQLitePath: dbPath})

	db, err := d.InitDB()
	if err != nil {
		t.Fatal(err)
	}

	if err := d.RunMigrations(context.Background()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	// Verify schema_migrations table
	var version string
	db.Raw("SELECT version FROM schema_migrations LIMIT 1").Scan(&version)
	if version != "1" {
		t.Errorf("version = %q, want %q", version, "1")
	}

	// Verify key tables exist (spot check)
	tables := []string{"api_keys", "providers", "usage_logs", "mcp_tool_call_logs_archive", "organizations"}
	for _, table := range tables {
		var count int64
		db.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&count)
		if count != 1 {
			t.Errorf("table %q not found", table)
		}
	}

	d.Shutdown(db)
	t.Log("OK: RunMigrations executed full schema successfully")
}

// splitSchemaStatements splits SQL text into individual statements,
// skipping comments and blank lines. PRAGMA statements are preserved.
func splitSchemaStatements(sql string) []string {
	var stmts []string
	var current strings.Builder

	for _, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)

		// Skip empty lines and full-line comments
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

	// Handle trailing statement without semicolon
	if current.Len() > 0 {
		s := strings.TrimSpace(current.String())
		if s != "" {
			stmts = append(stmts, s)
		}
	}

	return stmts
}

// truncStr truncates a string to at most n characters.
func truncStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
