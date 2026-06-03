package dialect

import (
	"os"
	"strconv"
	"testing"

	"gorm.io/gorm"
)

// envOr returns the environment variable value or the fallback.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envIntOr returns the environment variable value parsed as int, or the fallback.
func envIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// pgTestDSN returns the PostgreSQL connection URL from the environment.
func pgTestDSN() string {
	if dsn := os.Getenv("PG_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://crosslink:crosslink_test@localhost:5433/crosslink_test_pg?sslmode=disable&timezone=UTC"
}

// pgTestDBConfig builds a DBConfig for the test PG instance.
// All fields can be overridden via PG_TEST_HOST, PG_TEST_PORT,
// PG_TEST_USER, PG_TEST_PASSWORD, PG_TEST_DBNAME, PG_TEST_SSLMODE.
func pgTestDBConfig() DBConfig {
	return DBConfig{
		Driver:   "postgres",
		Host:     envOr("PG_TEST_HOST", "localhost"),
		Port:     envIntOr("PG_TEST_PORT", 5433),
		User:     envOr("PG_TEST_USER", "crosslink"),
		Password: envOr("PG_TEST_PASSWORD", "crosslink_test"),
		DBName:   envOr("PG_TEST_DBNAME", "crosslink_test_pg"),
		SSLMode:  envOr("PG_TEST_SSLMODE", "disable"),
		Timezone: envOr("PG_TEST_TIMEZONE", "UTC"),
	}
}

// mysqlTestDSN returns the MySQL connection string from the environment.
func mysqlTestDSN() string {
	if dsn := os.Getenv("MYSQL_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "root:crosslink_test@tcp(127.0.0.1:3307)/crosslink_test_mysql?charset=utf8mb4&parseTime=true&loc=UTC"
}

// mysqlTestDBConfig builds a DBConfig for the test MySQL instance.
// All fields can be overridden via MYSQL_TEST_HOST, MYSQL_TEST_PORT,
// MYSQL_TEST_USER, MYSQL_TEST_PASSWORD, MYSQL_TEST_DBNAME.
func mysqlTestDBConfig() DBConfig {
	return DBConfig{
		Driver:   "mysql",
		Host:     envOr("MYSQL_TEST_HOST", "127.0.0.1"),
		Port:     envIntOr("MYSQL_TEST_PORT", 3307),
		User:     envOr("MYSQL_TEST_USER", "root"),
		Password: envOr("MYSQL_TEST_PASSWORD", "crosslink_test"),
		DBName:   envOr("MYSQL_TEST_DBNAME", "crosslink_test_mysql"),
	}
}

// dropAllTablesPG drops all tables by recreating the public schema.
func dropAllTablesPG(t *testing.T, db *gorm.DB) {
	t.Helper()
	db.Exec("DROP SCHEMA public CASCADE")
	db.Exec("CREATE SCHEMA public")
}

// dropAllTablesMySQL drops all tables in the current MySQL database.
func dropAllTablesMySQL(t *testing.T, db *gorm.DB) {
	t.Helper()
	db.Exec("SET FOREIGN_KEY_CHECKS = 0")
	db.Exec("DROP PROCEDURE IF EXISTS drop_all_tables")
	db.Exec(`CREATE PROCEDURE drop_all_tables()
	BEGIN
		DECLARE done INT DEFAULT FALSE;
		DECLARE tname VARCHAR(128);
		DECLARE cur CURSOR FOR SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE();
		DECLARE CONTINUE HANDLER FOR NOT FOUND SET done = TRUE;
		OPEN cur;
		read_loop: LOOP
			FETCH cur INTO tname;
			IF done THEN LEAVE read_loop; END IF;
			SET @sql = CONCAT('DROP TABLE IF EXISTS ` + "`" + `' COLLATE utf8mb4_general_ci, tname, '` + "`" + `' COLLATE utf8mb4_general_ci);
			PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
		END LOOP;
		CLOSE cur;
	END`)
	db.Exec("CALL drop_all_tables()")
	db.Exec("DROP PROCEDURE IF EXISTS drop_all_tables")
	db.Exec("SET FOREIGN_KEY_CHECKS = 1")
}

