package dialect

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSQLiteDialect_LimitUpdate verifies that GORM's Limit+Update pattern works on SQLite.
// This is the pattern used by backfillLargeTableOrgID in phases.go:
//
//	db.Table(table).Where("org_id IS NULL").Limit(batchSize).Update("org_id", value)
//
// If this test passes, phases.go needs no changes for SQLite.
func TestSQLiteDialect_LimitUpdate(t *testing.T) {
	// RunMigrations reads from migrations/sqlite/ which is relative to project root.
	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)

	dir, err := os.MkdirTemp("", "crosslink-limit-*")
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
	defer d.Shutdown(db)

	if err := d.RunMigrations(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Insert a parent organization row so the FK on org_id is satisfied.
	db.Exec("INSERT INTO organizations (name, display_name, status) VALUES ('test-org', 'Test Org', 1)")

	// Insert 20 records with org_id = NULL.
	// Use NULL for FK columns (api_key_id, provider_id) since no parent rows exist.
	for i := 0; i < 20; i++ {
		db.Exec("INSERT INTO usage_logs (request_id, api_key_id, status_code, model_requested, model_used, route_type, created_at) VALUES ('req-1', NULL, 200, 'test', 'test', 'chat', ?)",
			time.Now().UTC())
	}

	// Pattern from phases.go: backfillLargeTableOrgID
	const batchSize = 5
	totalUpdated := 0
	for {
		result := db.Table("usage_logs").Where("org_id IS NULL").Limit(batchSize).Update("org_id", int64(1))
		if result.Error != nil {
			t.Fatalf("LIMIT+UPDATE: %v", result.Error)
		}
		if result.RowsAffected == 0 {
			break
		}
		totalUpdated += int(result.RowsAffected)
	}

	if totalUpdated != 20 {
		t.Errorf("totalUpdated = %d, want 20", totalUpdated)
	}

	// If this works, no need to modify phases.go.
	// If it fails, phases.go needs to use raw SQL with subquery instead.
	t.Logf("OK: LIMIT+UPDATE backfilled %d rows", totalUpdated)
}
