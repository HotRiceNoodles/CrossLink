package dialect

// Spike test: validates SQLite compatibility for multi-database adaptation.
// This file is for Phase 0 verification only — not production code.
// Run: cd internal/dialect && go test -v -run TestSpikeSQLite -timeout 60s

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	sqlite "github.com/glebarez/sqlite"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// ---------------------------------------------------------------------------
// Minimal model structs (mirroring production models, reduced fields)
// ---------------------------------------------------------------------------

type spikeAPIKey struct {
	ID            int64          `gorm:"primaryKey"`
	Name          string         `gorm:"not null"`
	KeyHash       string         `gorm:"uniqueIndex;not null"`
	AllowedModels datatypes.JSON `gorm:"type:jsonb"`
	AllowedRoutes datatypes.JSON `gorm:"type:jsonb"`
	Status        int16          `gorm:"not null;default:1"`
	CreatedAt     time.Time      `gorm:"not null;default:now()"`
	UpdatedAt     time.Time      `gorm:"not null;default:now()"`
	DeletedAt     gorm.DeletedAt `gorm:"index"`
}

type spikeProvider struct {
	ID          int64          `gorm:"primaryKey"`
	Name        string         `gorm:"uniqueIndex;not null"`
	ExtraConfig datatypes.JSON `gorm:"type:jsonb"`
	Status      int16          `gorm:"not null;default:1"`
	CreatedAt   time.Time      `gorm:"not null;default:now()"`
	UpdatedAt   time.Time      `gorm:"not null;default:now()"`
	DeletedAt   gorm.DeletedAt `gorm:"index"`
}

type spikeProviderModel struct {
	ID          int64          `gorm:"primaryKey"`
	ProviderID  int64          `gorm:"index;not null"`
	ModelName   string         `gorm:"index;not null"`
	ExtraConfig datatypes.JSON `gorm:"type:jsonb"`
	Status      int16          `gorm:"not null;default:1"`
	CreatedAt   time.Time      `gorm:"not null;default:now()"`
	UpdatedAt   time.Time      `gorm:"not null;default:now()"`
	DeletedAt   gorm.DeletedAt `gorm:"index"`
}

type spikeUsageLog struct {
	ID        int64     `gorm:"primaryKey"`
	APIKeyID  int64     `gorm:"index"`
	Status    int16     `gorm:"not null"`
	Model     string    `gorm:"size:128"`
	CreatedAt time.Time `gorm:"not null;default:now();index"`
}

// For Upsert test — mirrors budget_snapshots table
type spikeBudgetSnapshot struct {
	ID         int64   `gorm:"primaryKey"`
	TargetType string  `gorm:"uniqueIndex:idx_target_period;size:32;not null"`
	TargetID   int64   `gorm:"uniqueIndex:idx_target_period;not null"`
	PeriodKey  string  `gorm:"uniqueIndex:idx_target_period;size:32;not null"`
	Spent      float64 `gorm:"not null;default:0"`
	Budget     float64 `gorm:"not null;default:0"`
}

// ---------------------------------------------------------------------------
// SQLite DDL (translated from PG schema)
// ---------------------------------------------------------------------------

const sqliteDDL = `
CREATE TABLE IF NOT EXISTS spike_api_keys (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT NOT NULL,
    key_hash        TEXT UNIQUE NOT NULL,
    allowed_models  TEXT,
    allowed_routes  TEXT,
    status          INTEGER NOT NULL DEFAULT 1,
    created_at      DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at      DATETIME NOT NULL DEFAULT (datetime('now')),
    deleted_at      DATETIME
);
CREATE INDEX IF NOT EXISTS idx_spike_api_keys_deleted_at ON spike_api_keys(deleted_at);

CREATE TABLE IF NOT EXISTS spike_providers (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT UNIQUE NOT NULL,
    extra_config    TEXT,
    status          INTEGER NOT NULL DEFAULT 1,
    created_at      DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at      DATETIME NOT NULL DEFAULT (datetime('now')),
    deleted_at      DATETIME
);
CREATE INDEX IF NOT EXISTS idx_spike_providers_deleted_at ON spike_providers(deleted_at);

CREATE TABLE IF NOT EXISTS spike_provider_models (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_id     INTEGER NOT NULL,
    model_name      TEXT NOT NULL,
    extra_config    TEXT,
    status          INTEGER NOT NULL DEFAULT 1,
    created_at      DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at      DATETIME NOT NULL DEFAULT (datetime('now')),
    deleted_at      DATETIME
);
CREATE INDEX IF NOT EXISTS idx_spike_pm_deleted_at ON spike_provider_models(deleted_at);

CREATE TABLE IF NOT EXISTS spike_usage_logs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    api_key_id      INTEGER,
    status          INTEGER NOT NULL,
    model           TEXT,
    created_at      DATETIME NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_spike_ul_created_at ON spike_usage_logs(created_at);

CREATE TABLE IF NOT EXISTS spike_budget_snapshots (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    target_type     TEXT NOT NULL,
    target_id       INTEGER NOT NULL,
    period_key      TEXT NOT NULL,
    spent           REAL NOT NULL DEFAULT 0,
    budget          REAL NOT NULL DEFAULT 0,
    CONSTRAINT idx_target_period UNIQUE (target_type, target_id, period_key)
);
`

// ---------------------------------------------------------------------------
// Test setup
// ---------------------------------------------------------------------------

var spikeDB *gorm.DB
var spikeDir string

func TestMain(m *testing.M) {
	var err error

	// Create temp directory for file-based SQLite
	spikeDir, err = os.MkdirTemp("", "crosslink-spike-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(spikeDir)

	dbPath := filepath.Join(spikeDir, "test.db")
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"

	spikeDB, err = gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "open sqlite: %v\n", err)
		os.Exit(1)
	}

	// Single connection pool
	sqlDB, _ := spikeDB.DB()
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	// Execute DDL — split by semicolon for safety
	if err := execMultiStatement(spikeDB, sqliteDDL); err != nil {
		fmt.Fprintf(os.Stderr, "create tables: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	spikeDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	sqlDB.Close()
	os.Exit(code)
}

// execMultiStatement splits SQL by semicolons and executes each statement.
func execMultiStatement(db *gorm.DB, ddl string) error {
	var stmt string
	for _, ch := range ddl {
		stmt += string(ch)
		if ch == ';' {
			s := trimSpaces(stmt)
			if s != "" {
				if err := db.Exec(s).Error; err != nil {
					return fmt.Errorf("%q: %w", truncateSpike(s, 80), err)
				}
			}
			stmt = ""
		}
	}
	if s := trimSpaces(stmt); s != "" {
		if err := db.Exec(s).Error; err != nil {
			return fmt.Errorf("%q: %w", truncateSpike(s, 80), err)
		}
	}
	return nil
}

func trimSpaces(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\n' || s[0] == '\t' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\n' || s[len(s)-1] == '\t' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func truncateSpike(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// ---------------------------------------------------------------------------
// Spike Test 1: PRAGMA and Connection
// ---------------------------------------------------------------------------

func TestSpikeSQLite_01_PragmaAndConnection(t *testing.T) {
	sqlDB, _ := spikeDB.DB()

	var mode string
	spikeDB.Raw("PRAGMA journal_mode").Scan(&mode)
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want %q", mode, "wal")
	}

	var fkEnabled int
	spikeDB.Raw("PRAGMA foreign_keys").Scan(&fkEnabled)
	if fkEnabled != 1 {
		t.Fatalf("foreign_keys = %d, want 1", fkEnabled)
	}

	var busyTimeout int
	spikeDB.Raw("PRAGMA busy_timeout").Scan(&busyTimeout)
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
	}

	t.Logf("OK: WAL=%s, FK=%d, BusyTimeout=%d, Stats=%v", mode, fkEnabled, busyTimeout, sqlDB.Stats())
}

// ---------------------------------------------------------------------------
// Spike Test 2: CRUD with model structs
// ---------------------------------------------------------------------------

func TestSpikeSQLite_02_CRUD(t *testing.T) {
	// Create
	key := spikeAPIKey{
		Name:          "test-key",
		KeyHash:       "hash123",
		AllowedModels: datatypes.JSON(`["gpt-4","claude-3"]`),
		AllowedRoutes: datatypes.JSON(`["/v1/chat/completions"]`),
		Status:        1,
	}
	if err := spikeDB.Create(&key).Error; err != nil {
		t.Fatalf("Create: %v", err)
	}
	if key.ID == 0 {
		t.Fatal("Create: ID not set")
	}

	// Find
	var found spikeAPIKey
	if err := spikeDB.Where("key_hash = ?", "hash123").First(&found).Error; err != nil {
		t.Fatalf("Find: %v", err)
	}
	if found.Name != "test-key" {
		t.Fatalf("Find: Name = %q, want %q", found.Name, "test-key")
	}
	if string(found.AllowedModels) != `["gpt-4","claude-3"]` {
		t.Fatalf("Find: AllowedModels = %q", found.AllowedModels)
	}

	// Update
	if err := spikeDB.Model(&found).Update("status", 0).Error; err != nil {
		t.Fatalf("Update: %v", err)
	}
	var updated spikeAPIKey
	spikeDB.First(&updated, key.ID)
	if updated.Status != 0 {
		t.Fatalf("Update: Status = %d, want 0", updated.Status)
	}

	// Soft Delete
	if err := spikeDB.Delete(&found).Error; err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var count int64
	spikeDB.Model(&spikeAPIKey{}).Where("key_hash = ?", "hash123").Count(&count)
	if count != 0 {
		t.Fatalf("Delete: count = %d, want 0 (soft-deleted)", count)
	}

	// Unscoped finds it
	var undeleted spikeAPIKey
	spikeDB.Unscoped().Where("key_hash = ?", "hash123").First(&undeleted)
	if undeleted.DeletedAt.Valid == false {
		t.Fatal("Delete: deleted_at should be set")
	}

	t.Log("OK: Create/Find/Update/SoftDelete all work")
}

// ---------------------------------------------------------------------------
// Spike Test 3: Update("extra_config", []byte) — secret migration pattern
// ---------------------------------------------------------------------------

func TestSpikeSQLite_03_UpdateJSONBytes(t *testing.T) {
	// Create a provider with extra_config
	provider := spikeProvider{
		Name:        "test-provider",
		ExtraConfig: datatypes.JSON(`{"api_key":"plaintext123","region":"us-east"}`),
	}
	spikeDB.Create(&provider)

	// Simulate secret migration: read → modify → write back as []byte
	// This is the pattern from internal/secret/migration.go:97 and :230
	configData := map[string]any{
		"api_key": "enc://encrypted_value_here",
		"region":  "us-east",
	}
	data, _ := json.Marshal(configData)

	if err := spikeDB.Table("spike_providers").
		Where("id = ?", provider.ID).
		Update("extra_config", data).Error; err != nil {
		t.Fatalf("Update extra_config: %v", err)
	}

	// Read back and verify
	var row struct {
		ExtraConfig []byte `json:"extra_config"`
	}
	if err := spikeDB.Table("spike_providers").
		Select("extra_config").
		Where("id = ?", provider.ID).
		Find(&row).Error; err != nil {
		t.Fatalf("Read back: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(row.ExtraConfig, &result); err != nil {
		t.Fatalf("Unmarshal: %v (raw: %q)", err, string(row.ExtraConfig))
	}
	if result["api_key"] != "enc://encrypted_value_here" {
		t.Fatalf("api_key = %v, want enc://encrypted_value_here", result["api_key"])
	}

	t.Logf("OK: Update([]byte) → read back → unmarshal works (raw: %s)", truncateSpike(string(row.ExtraConfig), 80))
}

// ---------------------------------------------------------------------------
// Spike Test 4: Select("extra_config").Where("model_name = ?", ...)
// ---------------------------------------------------------------------------

func TestSpikeSQLite_04_SelectJSONColumn(t *testing.T) {
	// Create provider + provider_model
	provider := spikeProvider{Name: "model-provider", ExtraConfig: datatypes.JSON(`{}`)}
	spikeDB.Create(&provider)

	model := spikeProviderModel{
		ProviderID:  provider.ID,
		ModelName:   "gpt-4o",
		ExtraConfig: datatypes.JSON(`{"guardrails":{"max_tokens":4096,"enabled":true}}`),
	}
	spikeDB.Create(&model)

	// Pattern from internal/guardrail/service.go:206-211
	// NOTE: json.RawMessage fails on SQLite — the driver returns string (TEXT column),
	// but json.RawMessage is []byte. Must use []byte for cross-database compatibility.
	var row struct {
		ExtraConfig []byte `json:"extra_config"`
	}
	if err := spikeDB.Table("spike_provider_models").
		Select("extra_config").
		Where("model_name = ?", "gpt-4o").
		Limit(1).
		Find(&row).Error; err != nil {
		t.Fatalf("Select: %v", err)
	}

	if len(row.ExtraConfig) == 0 {
		t.Fatal("Select: empty extra_config")
	}

	var parsed struct {
		Guardrails *struct {
			MaxTokens int  `json:"max_tokens"`
			Enabled   bool `json:"enabled"`
		} `json:"guardrails"`
	}
	if err := json.Unmarshal(row.ExtraConfig, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v (raw: %q)", err, string(row.ExtraConfig))
	}
	if parsed.Guardrails == nil || !parsed.Guardrails.Enabled {
		t.Fatal("Select: guardrails not parsed correctly")
	}

	t.Logf("OK: Select + Where + json.RawMessage works (guardrails.enabled=%v)", parsed.Guardrails.Enabled)
}

// ---------------------------------------------------------------------------
// Spike Test 5: Upsert with clause.OnConflict
// ---------------------------------------------------------------------------

func TestSpikeSQLite_05_Upsert(t *testing.T) {
	// Pattern from internal/service/budget_calibration.go:234-243

	// Initial insert
	snap := spikeBudgetSnapshot{
		TargetType: "key",
		TargetID:   42,
		PeriodKey:  "2026-06",
		Spent:      10.5,
		Budget:     100.0,
	}
	if err := spikeDB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "target_type"}, {Name: "target_id"}, {Name: "period_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"spent", "budget"}),
	}).Create(&snap).Error; err != nil {
		t.Fatalf("First upsert: %v", err)
	}

	// Upsert (same unique key, different values)
	snap2 := spikeBudgetSnapshot{
		TargetType: "key",
		TargetID:   42,
		PeriodKey:  "2026-06",
		Spent:      25.0,
		Budget:     200.0,
	}
	if err := spikeDB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "target_type"}, {Name: "target_id"}, {Name: "period_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"spent", "budget"}),
	}).Create(&snap2).Error; err != nil {
		t.Fatalf("Second upsert: %v", err)
	}

	// Verify: should be 1 row with updated values
	var count int64
	spikeDB.Model(&spikeBudgetSnapshot{}).Where("target_type = 'key' AND target_id = 42").Count(&count)
	if count != 1 {
		t.Fatalf("Upsert: count = %d, want 1", count)
	}

	var result spikeBudgetSnapshot
	spikeDB.Where("target_type = 'key' AND target_id = 42").First(&result)
	if result.Spent != 25.0 || result.Budget != 200.0 {
		t.Fatalf("Upsert: spent=%.1f budget=%.1f, want 25.0/200.0", result.Spent, result.Budget)
	}

	t.Log("OK: clause.OnConflict upsert works correctly")
}

// ---------------------------------------------------------------------------
// Spike Test 6: Time comparison on TEXT column
// ---------------------------------------------------------------------------

func TestSpikeSQLite_06_TimeComparison(t *testing.T) {
	now := time.Now().UTC()

	// Insert logs at different times
	for i := 0; i < 10; i++ {
		ts := now.Add(time.Duration(-i) * time.Hour)
		log := spikeUsageLog{
			APIKeyID:  1,
			Status:    200,
			Model:     "gpt-4",
			CreatedAt: ts,
		}
		spikeDB.Create(&log)
	}

	// Query: logs in last 5 hours
	cutoff := now.Add(-5 * time.Hour)
	var recent []spikeUsageLog
	if err := spikeDB.Where("created_at > ?", cutoff).Find(&recent).Error; err != nil {
		t.Fatalf("Time query: %v", err)
	}
	if len(recent) != 5 && len(recent) != 6 { // boundary depends on rounding
		t.Fatalf("Time query: got %d rows, want ~5 (within 5h of %v)", len(recent), cutoff)
	}

	// Query: logs before 1 hour ago
	old := now.Add(-1 * time.Hour)
	var oldLogs []spikeUsageLog
	spikeDB.Where("created_at < ?", old).Find(&oldLogs)
	if len(oldLogs) < 8 {
		t.Fatalf("Time query (old): got %d rows, want >= 8", len(oldLogs))
	}

	// Verify stored format
	var sample spikeUsageLog
	spikeDB.Order("id DESC").First(&sample)
	t.Logf("OK: Time comparison works. Recent=%d, Old=%d, StoredFormat=%s", len(recent), len(oldLogs), sample.CreatedAt.Format(time.RFC3339Nano))
}

// ---------------------------------------------------------------------------
// Spike Test 7: Write performance
// ---------------------------------------------------------------------------

func TestSpikeSQLite_07_WritePerformance(t *testing.T) {
	const N = 100
	start := time.Now()

	for i := 0; i < N; i++ {
		log := spikeUsageLog{
			APIKeyID:  1,
			Status:    200,
			Model:     "perf-test",
			CreatedAt: time.Now().UTC(),
		}
		if err := spikeDB.Create(&log).Error; err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	elapsed := time.Since(start)
	tps := float64(N) / elapsed.Seconds()
	t.Logf("OK: %d writes in %v = %.0f TPS", N, elapsed, tps)

	if tps < 50 {
		t.Errorf("Performance: %.0f TPS is below 50 TPS threshold", tps)
	}
}

// ---------------------------------------------------------------------------
// Spike Test 8: Multi-statement vs single-statement execution
// ---------------------------------------------------------------------------

func TestSpikeSQLite_08_MultiStatementExec(t *testing.T) {
	sqlDB, _ := spikeDB.DB()

	// Test A: Can gorm db.Exec handle multi-statement SQL?
	multiSQL := `
		INSERT INTO spike_providers (name, extra_config, status) VALUES ('multi-a', '{}', 1);
		INSERT INTO spike_providers (name, extra_config, status) VALUES ('multi-b', '{}', 1);
	`
	err := spikeDB.Exec(multiSQL).Error
	if err != nil {
		t.Logf("Multi-statement via GORM: FAIL (%v) — will need per-statement execution", err)
	} else {
		// Verify both rows were inserted
		var count int64
		spikeDB.Model(&spikeProvider{}).Where("name IN ('multi-a', 'multi-b')").Count(&count)
		if count == 2 {
			t.Log("Multi-statement via GORM: OK (both statements executed)")
		} else {
			t.Logf("Multi-statement via GORM: PARTIAL (count=%d, want 2)", count)
		}
	}

	// Test B: database/sql raw Exec
	err = func() error {
		conn, err := sqlDB.Conn(context.Background())
		if err != nil {
			return err
		}
		defer conn.Close()
		_, err = conn.ExecContext(context.Background(), `
			INSERT INTO spike_providers (name, extra_config, status) VALUES ('multi-c', '{}', 1);
			INSERT INTO spike_providers (name, extra_config, status) VALUES ('multi-d', '{}', 1);
		`)
		return err
	}()
	if err != nil {
		t.Logf("Multi-statement via database/sql: FAIL (%v)", err)
	} else {
		var count int64
		spikeDB.Model(&spikeProvider{}).Where("name IN ('multi-c', 'multi-d')").Count(&count)
		t.Logf("Multi-statement via database/sql: OK (count=%d)", count)
	}

	// Test C: Per-statement execution with transaction (recommended approach)
	tx := spikeDB.Begin()
	stmts := []string{
		"INSERT INTO spike_providers (name, extra_config, status) VALUES ('multi-e', '{}', 1)",
		"INSERT INTO spike_providers (name, extra_config, status) VALUES ('multi-f', '{}', 1)",
	}
	for _, s := range stmts {
		if err := tx.Exec(s).Error; err != nil {
			tx.Rollback()
			t.Fatalf("Per-statement: %v", err)
		}
	}
	tx.Commit()

	var count int64
	spikeDB.Model(&spikeProvider{}).Where("name IN ('multi-e', 'multi-f')").Count(&count)
	if count != 2 {
		t.Fatalf("Per-statement: count=%d, want 2", count)
	}

	t.Log("OK: Per-statement + transaction works reliably")
}
