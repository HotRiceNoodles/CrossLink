package dialect

import (
	"fmt"
	"os"
	"testing"

	"gorm.io/gorm"
)

// pgTestDSN returns the PostgreSQL connection URL from the environment.
func pgTestDSN() string {
	if dsn := os.Getenv("PG_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://crosslink:crosslink_test@localhost:5433/crosslink_test_pg?sslmode=disable"
}

// pgTestDBConfig builds a DBConfig for the test PG instance.
func pgTestDBConfig() DBConfig {
	return DBConfig{
		Driver:   "postgres",
		Host:     "localhost",
		Port:     5433,
		User:     "crosslink",
		Password: "crosslink_test",
		DBName:   "crosslink_test_pg",
		SSLMode:  "disable",
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
func mysqlTestDBConfig() DBConfig {
	return DBConfig{
		Driver:   "mysql",
		Host:     "127.0.0.1",
		Port:     3307,
		User:     "root",
		Password: "crosslink_test",
		DBName:   "crosslink_test_mysql",
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

// Suppress unused-import warning for fmt.
var _ = fmt.Sprintf
