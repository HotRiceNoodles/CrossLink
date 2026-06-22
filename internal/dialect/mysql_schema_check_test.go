package dialect

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stripComments removes full-line comments and blank lines from SQL text,
// returning only executable SQL lines. This is used to check for PG-isms
// only in actual SQL, not in documentation comments.
func stripComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// TestMySQLSchema_SyntaxCheck performs a dry-run syntax check by parsing the
// schema without executing it. This is useful when MySQL is not available.
func TestMySQLSchema_SyntaxCheck(t *testing.T) {
	schemaPath := filepath.Join("..", "..", "migrations", "mysql", "000001_init_schema.up.sql")
	sqlBytes, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}

	sql := string(sqlBytes)
	statements := splitSchemaStatements(sql)

	// Basic syntax checks
	tableCount := 0
	indexCount := 0
	for _, stmt := range statements {
		upper := strings.ToUpper(stmt)
		if strings.HasPrefix(upper, "CREATE TABLE") {
			tableCount++
		}
		if strings.HasPrefix(upper, "CREATE INDEX") || strings.HasPrefix(upper, "CREATE UNIQUE INDEX") {
			indexCount++
		}
	}

	if tableCount != 37 {
		t.Errorf("expected 37 CREATE TABLE statements, got %d", tableCount)
	}
	if indexCount == 0 {
		t.Error("expected some CREATE INDEX statements, got 0")
	}

	// Verify critical patterns
	checks := []struct {
		pattern string
		desc    string
	}{
		{"ENGINE=InnoDB", "ENGINE=InnoDB clause"},
		{"DEFAULT CHARSET=utf8mb4", "charset utf8mb4"},
		{"DATETIME(3)", "DATETIME(3) timestamps"},
		{"CURRENT_TIMESTAMP(3)", "CURRENT_TIMESTAMP(3) defaults"},
		{"PARTITION BY RANGE COLUMNS", "partitioning on mcp_tool_call_logs"},
		{"PARTITION p_future", "catch-all partition"},
		{"BIGINT AUTO_INCREMENT", "auto-increment IDs"},
		{"COLLATE=utf8mb4_unicode_ci", "unicode collation"},
	}
	for _, c := range checks {
		if !strings.Contains(sql, c.pattern) {
			t.Errorf("schema missing %s (%q)", c.desc, c.pattern)
		}
	}

	// Verify no PG-isms leaked through (check only statements, not comments)
	onlySQL := stripComments(sql)
	badPatterns := []struct {
		pattern string
		desc    string
	}{
		{"BIGSERIAL", "BIGSERIAL (PG type)"},
		{"TIMESTAMPTZ", "TIMESTAMPTZ (PG type)"},
		{"JSONB", "JSONB (PG type)"},
		{"::jsonb", "::jsonb cast"},
		{"NOW()", "NOW() (PG function)"},
		{"BOOLEAN", "BOOLEAN (PG type)"},
		{"CONCURRENTLY", "CONCURRENTLY (PG index)"},
	}
	for _, b := range badPatterns {
		if strings.Contains(onlySQL, b.pattern) {
			t.Errorf("schema contains %s (%q) — not valid MySQL", b.desc, b.pattern)
		}
	}

	t.Logf("OK: %d statements, %d tables, %d indexes, all patterns verified", len(statements), tableCount, indexCount)
}

// TestMySQLSchema_DownSyntaxCheck validates the down migration file structure.
func TestMySQLSchema_DownSyntaxCheck(t *testing.T) {
	downPath := filepath.Join("..", "..", "migrations", "mysql", "000001_init_schema.down.sql")
	sqlBytes, err := os.ReadFile(downPath)
	if err != nil {
		t.Fatalf("read down schema: %v", err)
	}

	sql := string(sqlBytes)
	statements := splitSchemaStatements(sql)

	dropCount := 0
	for _, stmt := range statements {
		if strings.HasPrefix(strings.ToUpper(stmt), "DROP TABLE") {
			dropCount++
		}
	}

	if dropCount != 37 {
		t.Errorf("expected 37 DROP TABLE statements, got %d", dropCount)
	}

	// Verify mcp_tool_call_logs_archive is first (most dependent)
	first := strings.TrimSpace(statements[0])
	if !strings.Contains(first, "mcp_tool_call_logs_archive") {
		t.Errorf("first DROP should be mcp_tool_call_logs_archive, got: %s", truncStr(first, 80))
	}

	// Verify system_settings is last (least dependent)
	last := strings.TrimSpace(statements[len(statements)-1])
	if !strings.Contains(last, "system_settings") {
		t.Errorf("last DROP should be system_settings, got: %s", truncStr(last, 80))
	}

	t.Logf("OK: %d DROP TABLE statements in correct dependency order", dropCount)
}

// TestMySQLSchema_PartitionSyntax validates the partition definition for mcp_tool_call_logs.
func TestMySQLSchema_PartitionSyntax(t *testing.T) {
	schemaPath := filepath.Join("..", "..", "migrations", "mysql", "000001_init_schema.up.sql")
	sqlBytes, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}

	sql := string(sqlBytes)

	// Must have 12 monthly partitions + 1 catch-all
	months := []string{
		"p_2026_01", "p_2026_02", "p_2026_03", "p_2026_04",
		"p_2026_05", "p_2026_06", "p_2026_07", "p_2026_08",
		"p_2026_09", "p_2026_10", "p_2026_11", "p_2026_12",
	}
	for _, m := range months {
		if !strings.Contains(sql, fmt.Sprintf("PARTITION %s", m)) {
			t.Errorf("missing monthly partition %s", m)
		}
	}

	if !strings.Contains(sql, "PARTITION p_future") {
		t.Error("missing catch-all partition p_future")
	}

	// Verify boundary format uses DATETIME(3) precision
	if !strings.Contains(sql, "'2026-02-01 00:00:00.000'") {
		t.Error("partition boundaries should use .000 precision")
	}

	t.Log("OK: 12 monthly partitions + p_future catch-all verified")
}
