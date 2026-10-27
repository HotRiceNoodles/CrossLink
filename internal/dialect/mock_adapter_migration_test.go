package dialect

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// 在临时表上验证真实 PostgreSQL 迁移，不修改测试库中的应用表。
func TestPostgresMockAdapterMigration(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("需要显式提供 PG_TEST_DSN")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	exec := func(query string) {
		t.Helper()
		if _, err := tx.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	apply := func(name string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "..", "migrations", "postgres", name))
		if err != nil {
			t.Fatal(err)
		}
		exec(string(data))
	}
	reject := func(query string) {
		t.Helper()
		exec("SAVEPOINT rejected_write")
		if _, err := tx.Exec(query); err == nil {
			t.Fatal("应拒绝不受支持的适配器或不安全回滚")
		}
		exec("ROLLBACK TO SAVEPOINT rejected_write")
	}
	exec("CREATE TEMP TABLE providers (adapter_type TEXT NOT NULL) ON COMMIT DROP")
	apply("000003_adapter_type_validation.up.sql")
	exec("INSERT INTO providers VALUES ('openai_compatible')")
	reject("INSERT INTO providers VALUES ('mock')")
	apply("000085_drop_stale_adapter_type_check.up.sql")
	exec("INSERT INTO providers VALUES ('mock')")
	// 适配器合法性由运行时注册表验证，数据库不再维护易过期的白名单。
	exec("INSERT INTO providers VALUES ('unregistered-adapter')")
	exec("DELETE FROM providers WHERE adapter_type='unregistered-adapter'")
	down, err := os.ReadFile(filepath.Join("..", "..", "migrations", "postgres", "000085_drop_stale_adapter_type_check.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	reject(string(down))
	var count int
	if err := tx.QueryRow("SELECT COUNT(*) FROM providers WHERE adapter_type='mock'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("回滚失败后应保留 mock 供应商")
	}
	exec("DELETE FROM providers WHERE adapter_type='mock'")
	exec(string(down))
	reject("INSERT INTO providers VALUES ('mock')")
	apply("000085_drop_stale_adapter_type_check.up.sql")
	exec("INSERT INTO providers VALUES ('mock')")
}
