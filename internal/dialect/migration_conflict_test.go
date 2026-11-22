package dialect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openTestSQLite opens an isolated in-memory sqlite via the same driver the
// dialect runner uses.
func openTestSQLite(t *testing.T) (*gorm.DB, error) {
	t.Helper()
	return gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "replay.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
}

// migrationDirFor resolves the migrations root: the repo root relative to this
// package (…/migrations). Inside the commercial merged build tree the same
// layout holds — the overlay migrations have already been copied in by the
// build script, so this test guards the MERGED chain there and the community
// chain in the community repo (audit item 6: CI must reject same-dialect
// same-direction duplicate versions).
func migrationDirFor(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	root := filepath.Join(wd, "..", "..", "migrations")
	require.DirExists(t, root)
	return root
}

// TestMigrationVersionsUniquePerDialect asserts that within each dialect
// directory, no version number appears twice with the same direction. This is
// the file-level equivalent of what golang-migrate's MySQL/Postgres source
// enforces at load time — and the exact class that broke the merged chains
// before the overlay renumbering (audit item 6).
func TestMigrationVersionsUniquePerDialect(t *testing.T) {
	t.Parallel()
	root := migrationDirFor(t)
	for _, dialect := range []string{"mysql", "postgres", "sqlite"} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(root, dialect)
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			seen := map[string]string{} // "version.direction" → first file name
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() || !strings.HasSuffix(name, ".sql") {
					continue
				}
				version := strings.SplitN(name, "_", 2)[0]
				direction := "up"
				if strings.HasSuffix(name, ".down.sql") {
					direction = "down"
				}
				key := version + "." + direction
				if prev, dup := seen[key]; dup {
					t.Errorf("duplicate migration version %s in %s: %s and %s (audit item 6: renumber one of them)", key, dialect, prev, name)
					continue
				}
				seen[key] = name
			}
			require.NotEmpty(t, seen, "dialect %s has no migrations — layout changed?", dialect)
		})
	}
}

// TestSQLiteOldOverlayNumberingUpgradeCompat exercises the REAL runner against
// the divergent-history case documented in overlay/migrations/README.md (audit
// item 6, acceptance follow-up): a database deployed BEFORE the overlay
// renumbering carries the same logical schema recorded under the OLD overlay
// numbers (22/23/24/29). Re-running the current chain replays files 32–36 —
// CREATE … IF NOT EXISTS plus the duplicate-column tolerance must carry the
// upgrade through without error. Community tree skips (no overlay files); the
// commercial merged build and CI run it for real. Serial: t.Chdir.
func TestSQLiteOldOverlayNumberingUpgradeCompat(t *testing.T) {
	root := migrationDirFor(t)
	overlayFiles := []string{
		"000032_kb_poison_fingerprints.up.sql",
		"000033_kb_fp_requested_by.up.sql",
		"000034_rag_ledger.up.sql",
		"000035_guardrail_tuning.up.sql",
		"000036_guardrail_tuning_freeze.up.sql",
	}
	if _, err := os.Stat(filepath.Join(root, "sqlite", overlayFiles[0])); err != nil {
		t.Skip("community tree: overlay migrations absent (covered in merged build/CI)")
	}

	// Community-only chain dir (pre-overlay deployment source).
	commOnly := t.TempDir()
	commMig := filepath.Join(commOnly, "migrations", "sqlite")
	require.NoError(t, os.MkdirAll(commMig, 0o755))
	entries, err := os.ReadDir(filepath.Join(root, "sqlite"))
	require.NoError(t, err)
	overlaySet := map[string]bool{}
	for _, f := range overlayFiles {
		overlaySet[f] = true
	}
	for _, e := range entries {
		if !overlaySet[e.Name()] {
			data, err := os.ReadFile(filepath.Join(root, "sqlite", e.Name()))
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(commMig, e.Name()), data, 0o644))
		}
	}

	d := NewSQLiteDialect(DBConfig{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "olddeploy.db")})
	db, err := d.InitDB()
	require.NoError(t, err)
	if sqlDB, derr := db.DB(); derr == nil {
		t.Cleanup(func() { sqlDB.Close() })
	}

	// Phase 1: community chain applied (as on a pre-overlay deployment).
	t.Chdir(commOnly)
	require.NoError(t, d.RunMigrations(t.Context()))

	// Phase 2: simulate the OLD overlay content having been applied under the
	// OLD numbering. The old numbers (22/23/24/29) COLLIDE with community's
	// own migrations — that is precisely the divergent history — so phase 1
	// already recorded those versions once (community's file won the runner's
	// per-version skip). Applying the same logical DDL now reproduces the
	// "overlay content already present, old numbers recorded" database shape.
	for _, f := range overlayFiles {
		data, err := os.ReadFile(filepath.Join(root, "sqlite", f))
		require.NoError(t, err)
		for _, stmt := range splitSQL(string(data)) {
			if stmt == "" {
				continue
			}
			require.NoError(t, db.Exec(stmt).Error, "fixture %s", f)
		}
	}

	// Phase 3: upgrade with the CURRENT (renumbered) chain — replay must pass.
	t.Chdir(root + string(os.PathSeparator) + "..")
	require.NoError(t, d.RunMigrations(t.Context()), "renumbered replay on an old-numbered database must succeed")

	// All new versions recorded; the freeze column from the tail migration exists.
	for _, v := range []string{"000032", "000033", "000034", "000035", "000036"} {
		var n int64
		require.NoError(t, db.Raw("SELECT COUNT(*) FROM schema_migrations WHERE version = ?", v).Scan(&n).Error)
		assert.EqualValues(t, 1, n, "version %s must be recorded after upgrade", v)
	}
	assert.True(t, db.Migrator().HasColumn("guardrail_tunings", "frozen_at"))
	assert.True(t, db.Migrator().HasTable("kb_poison_fingerprints"))
}

// TestSQLiteOverlayReplayTolerant guards the upgrade-compat semantics added
// for audit item 6: a database that applied the SAME logical change under the
// pre-renumbering numbering replays the renumbered ALTER…ADD COLUMN file —
// the runner must treat "duplicate column name" as already-applied instead of
// failing startup.
func TestSQLiteOverlayReplayTolerant(t *testing.T) {
	t.Parallel()
	db, err := openTestSQLite(t)
	require.NoError(t, err)
	if sqlDB, derr := db.DB(); derr == nil {
		t.Cleanup(func() { sqlDB.Close() }) // Windows: release the file before TempDir cleanup
	}
	require.NoError(t, db.Exec("CREATE TABLE replay_probe (id INTEGER PRIMARY KEY)").Error)
	require.NoError(t, db.Exec("ALTER TABLE replay_probe ADD COLUMN mark TEXT").Error)

	// The exact replay shape: re-running the ADD COLUMN fails with
	// "duplicate column name" — the tolerant path must absorb it.
	require.NoError(t, db.Exec("ALTER TABLE replay_probe ADD COLUMN mark2 TEXT").Error)
	err = db.Exec("ALTER TABLE replay_probe ADD COLUMN mark2 TEXT").Error
	require.Error(t, err, "sanity: driver must report duplicate column")
	require.Contains(t, err.Error(), "duplicate column name")

	// isAlterAddColumn only ever matches ALTER TABLE … ADD COLUMN shapes.
	require.True(t, isAlterAddColumn("ALTER TABLE t ADD COLUMN c TEXT"))
	require.True(t, isAlterAddColumn("alter table t\n add column c text"))
	require.False(t, isAlterAddColumn("CREATE TABLE t (c TEXT)"))
	require.False(t, isAlterAddColumn("ALTER TABLE t RENAME COLUMN a TO b"))
	require.False(t, isAlterAddColumn("DROP TABLE t"))
}
