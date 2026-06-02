//go:build mysql

package dialect

import (
	"os"
	"path/filepath"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// mysqlDSN returns the MySQL connection string from the environment.
// Defaults to root:root@tcp(127.0.0.1:3306)/crosslink_test if not set.
//
// Setup:
//
//	docker run -d --name mysql-test -e MYSQL_ROOT_PASSWORD=root -e MYSQL_DATABASE=crosslink_test -p 3306:3306 mysql:8.0
//	export MYSQL_DSN="root:root@tcp(127.0.0.1:3306)/crosslink_test?charset=utf8mb4&parseTime=true&loc=UTC"
//	go test ./internal/dialect/ -tags=mysql -v -run TestMySQLSchema
func mysqlDSN() string {
	if dsn := os.Getenv("MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return "root:root@tcp(127.0.0.1:3306)/crosslink_test?charset=utf8mb4&parseTime=true&loc=UTC"
}

// TestMySQLSchema_Up validates that the full MySQL schema file loads
// without errors into a MySQL database.
func TestMySQLSchema_Up(t *testing.T) {
	sqlBytes, err := os.ReadFile(filepath.Join("..", "..", "migrations", "mysql", "000001_init_schema.up.sql"))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}

	db, err := gorm.Open(mysql.Open(mysqlDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Skipf("MySQL not available: %v", err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	// Drop all tables first for a clean slate
	dropAllTablesMySQL(t, db)

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
		db.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?", table).Scan(&count)
		if count != 1 {
			t.Errorf("table %q not found in information_schema", table)
		}
	}

	// Verify key indexes exist
	indexes := []string{
		"idx_api_key_hashes_key_hash",
		"idx_usage_logs_created_at",
		"provider_models_active_unique",
		"organizations_name_active_idx",
		"idx_agent_fingerprints_dedup",
		"idx_insights_unique",
		"idx_mcp_perm_principal",
		"idx_mcp_logs_time",
	}
	for _, idx := range indexes {
		var count int64
		db.Raw("SELECT count(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND index_name = ?", idx).Scan(&count)
		if count < 1 {
			t.Errorf("index %q not found in information_schema", idx)
		}
	}

	// Verify mcp_tool_call_logs has partitions
	var partCount int64
	db.Raw("SELECT count(*) FROM information_schema.partitions WHERE table_schema = DATABASE() AND table_name = 'mcp_tool_call_logs'").Scan(&partCount)
	if partCount < 13 { // 12 monthly + 1 catch-all
		t.Errorf("mcp_tool_call_logs partition count = %d, want >= 13", partCount)
	}

	t.Logf("OK: %d tables, %d key indexes, %d partitions verified", len(tables), len(indexes), partCount)
}

// TestMySQLSchema_Down validates that the down migration drops all tables cleanly.
func TestMySQLSchema_Down(t *testing.T) {
	// First, apply the up migration
	sqlUp, err := os.ReadFile(filepath.Join("..", "..", "migrations", "mysql", "000001_init_schema.up.sql"))
	if err != nil {
		t.Fatalf("read up schema: %v", err)
	}

	db, err := gorm.Open(mysql.Open(mysqlDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Skipf("MySQL not available: %v", err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	// Drop all tables first for a clean slate
	dropAllTablesMySQL(t, db)

	for _, stmt := range splitSchemaStatements(string(sqlUp)) {
		if stmt == "" {
			continue
		}
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("up statement: %v", err)
		}
	}

	// Now apply the down migration
	sqlDown, err := os.ReadFile(filepath.Join("..", "..", "migrations", "mysql", "000001_init_schema.down.sql"))
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

	// Verify no user tables remain
	var count int64
	db.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema = DATABASE()").Scan(&count)
	if count != 0 {
		var names []string
		db.Raw("SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE()").Scan(&names)
		t.Fatalf("expected 0 tables after down migration, got %d: %v", count, names)
	}

	t.Log("OK: down migration drops all tables cleanly")
}

// dropAllTablesMySQL drops all tables in the current database for a clean slate.
// It disables FK checks temporarily to allow dropping in any order.
func dropAllTablesMySQL(t *testing.T, db *gorm.DB) {
	t.Helper()
	db.Exec("SET FOREIGN_KEY_CHECKS = 0")

	// Use a stored procedure to iterate and drop all tables
	db.Exec("DROP PROCEDURE IF EXISTS drop_all_tables")
	sql := `
	CREATE PROCEDURE drop_all_tables()
	BEGIN
		DECLARE done INT DEFAULT FALSE;
		DECLARE tname VARCHAR(128);
		DECLARE cur CURSOR FOR SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE();
		DECLARE CONTINUE HANDLER FOR NOT FOUND SET done = TRUE;
		OPEN cur;
		read_loop: LOOP
			FETCH cur INTO tname;
			IF done THEN
				LEAVE read_loop;
			END IF;
			SET @sql = CONCAT('DROP TABLE IF EXISTS ` + "`" + `' COLLATE utf8mb4_general_ci, tname, '` + "`" + `' COLLATE utf8mb4_general_ci);
			PREPARE stmt FROM @sql;
			EXECUTE stmt;
			DEALLOCATE PREPARE stmt;
		END LOOP;
		CLOSE cur;
	END`
	db.Exec(sql)
	db.Exec("CALL drop_all_tables()")
	db.Exec("DROP PROCEDURE IF EXISTS drop_all_tables")
	db.Exec("SET FOREIGN_KEY_CHECKS = 1")
}
