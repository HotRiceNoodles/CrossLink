package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	sqlite "github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ── helpers ──

func setupSsrfAllowlistDB(t *testing.T) *gorm.DB {
	t.Helper()
	// Shared-cache in-memory DB: gorm's pool opens several connections, and a
	// plain ":memory:" DSN gives each its own EMPTY database (concurrent-test
	// writes would hit "no such table").
	db, err := gorm.Open(sqlite.Open("file:ssrf_e2e_mem?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.Exec("DROP TABLE IF EXISTS ssrf_allowlist_configs").Error)
	require.NoError(t, db.Exec("DROP TABLE IF EXISTS audit_logs").Error)
	require.NoError(t, db.AutoMigrate(&model.SsrfAllowlistConfig{}, &model.AuditLog{}))
	return db
}

func strictSnapshot(t *testing.T) {
	t.Helper()
	// Snapshot of the guardrail policy WITHOUT widening it between tests:
	// every test re-publishes what it needs, and this restores strict at
	// cleanup like guardrail's restoreStrict does for its own package.
	t.Cleanup(func() { guardrail.ApplyInternalAllowlist(nil) })
}

func effectiveCIDRs() []string {
	out := []string{}
	for _, p := range guardrail.InternalAllowlistSnapshot() {
		out = append(out, p.String())
	}
	return out
}

func actor() SsrfAllowlistActor {
	return SsrfAllowlistActor{UserID: 7, Username: "admin-op", IP: "10.0.0.9", UserAgent: "test"}
}

func insertRow(t *testing.T, db *gorm.DB, cidrsJSON string, version int64) {
	t.Helper()
	require.NoError(t, db.Create(&model.SsrfAllowlistConfig{
		ID: 1, CIDRsJSON: cidrsJSON, Version: version, UpdatedBy: 9, UpdatedByName: "seed", UpdatedAt: time.Now().UTC(),
	}).Error)
}

func countRows(t *testing.T, db *gorm.DB, table string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Table(table).Count(&n).Error)
	return n
}

// ── cold start: precedence db > env > yaml > strict, fail-closed ──

func TestSsrfAllowlist_ColdStart_Precedence(t *testing.T) {
	strictSnapshot(t)

	t.Run("no row + env value wins", func(t *testing.T) {
		db := setupSsrfAllowlistDB(t)
		svc := NewSsrfAllowlistService(db, nil, []string{"10.1.0.0/16"}, true)
		svc.LoadColdStart(context.Background())
		st, _ := svc.Status(context.Background())
		assert.Equal(t, SsrfSourceEnv, st.Source)
		assert.Equal(t, []string{"10.1.0.0/16"}, effectiveCIDRs())
	})

	t.Run("no row + yaml value", func(t *testing.T) {
		db := setupSsrfAllowlistDB(t)
		svc := NewSsrfAllowlistService(db, nil, []string{"10.2.0.0/16"}, false)
		svc.LoadColdStart(context.Background())
		st, _ := svc.Status(context.Background())
		assert.Equal(t, SsrfSourceYAML, st.Source)
		assert.Equal(t, []string{"10.2.0.0/16"}, effectiveCIDRs())
	})

	t.Run("no row + nothing = strict", func(t *testing.T) {
		db := setupSsrfAllowlistDB(t)
		svc := NewSsrfAllowlistService(db, nil, nil, false)
		svc.LoadColdStart(context.Background())
		st, _ := svc.Status(context.Background())
		assert.Equal(t, SsrfSourceStrict, st.Source)
		assert.Empty(t, effectiveCIDRs())
	})

	t.Run("db row overrides env", func(t *testing.T) {
		db := setupSsrfAllowlistDB(t)
		insertRow(t, db, `["172.20.0.0/14"]`, 3)
		svc := NewSsrfAllowlistService(db, nil, []string{"10.1.0.0/16"}, true)
		svc.LoadColdStart(context.Background())
		st, _ := svc.Status(context.Background())
		assert.Equal(t, SsrfSourceDB, st.Source)
		assert.Equal(t, int64(3), st.Version)
		assert.Equal(t, []string{"172.20.0.0/14"}, effectiveCIDRs())
	})

	t.Run("db row explicit empty array = strict override, env NOT applied", func(t *testing.T) {
		db := setupSsrfAllowlistDB(t)
		insertRow(t, db, `[]`, 2)
		svc := NewSsrfAllowlistService(db, nil, []string{"10.1.0.0/16"}, true)
		svc.LoadColdStart(context.Background())
		st, _ := svc.Status(context.Background())
		assert.Equal(t, SsrfSourceDB, st.Source) // override ACTIVE, just strict
		assert.Empty(t, effectiveCIDRs())
		assert.Equal(t, SsrfSourceEnv, st.FallbackSource)
	})

	t.Run("invalid stored row = strict + degraded, never wider env", func(t *testing.T) {
		db := setupSsrfAllowlistDB(t)
		insertRow(t, db, `["8.8.8.8/32"]`, 1) // public CIDR not allowlistable
		svc := NewSsrfAllowlistService(db, nil, []string{"10.1.0.0/16"}, true)
		svc.LoadColdStart(context.Background())
		st, _ := svc.Status(context.Background())
		assert.Equal(t, SsrfSourceStrict, st.Source)
		assert.Empty(t, effectiveCIDRs())
		assert.Contains(t, st.Degraded, "invalid")
	})

	t.Run("garbage JSON row = strict + degraded", func(t *testing.T) {
		db := setupSsrfAllowlistDB(t)
		insertRow(t, db, `{"not":"an array"}`, 1)
		svc := NewSsrfAllowlistService(db, nil, nil, false)
		svc.LoadColdStart(context.Background())
		st, _ := svc.Status(context.Background())
		assert.Equal(t, SsrfSourceStrict, st.Source)
		assert.NotEmpty(t, st.Degraded)
	})

	t.Run("db read failure = strict + degraded (fail closed)", func(t *testing.T) {
		db := setupSsrfAllowlistDB(t)
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close()) // subsequent reads fail
		svc := NewSsrfAllowlistService(db, nil, []string{"10.1.0.0/16"}, true)
		svc.LoadColdStart(context.Background())
		st, _ := svc.Status(context.Background())
		assert.Equal(t, SsrfSourceStrict, st.Source)
		assert.Empty(t, effectiveCIDRs())
		assert.Contains(t, st.Degraded, "database read failed")
	})
}

// ── Apply ──

func TestSsrfAllowlist_Apply_HappyPath(t *testing.T) {
	strictSnapshot(t)
	db := setupSsrfAllowlistDB(t)
	svc := NewSsrfAllowlistService(db, nil, nil, false)

	res, serr := svc.Apply(context.Background(), []string{" 10.30.0.0/16 ", "10.31.0.0/16"}, 0, actor())
	require.Nil(t, serr)
	assert.Equal(t, int64(1), res.Version)
	assert.Equal(t, []string{"10.30.0.0/16", "10.31.0.0/16"}, res.EffectiveCIDRs) // trimmed + canonical
	assert.Equal(t, SsrfSourceDB, res.Source)
	assert.Equal(t, "ttl-only", res.Broadcast)
	// Memory follows the committed row.
	assert.Equal(t, []string{"10.30.0.0/16", "10.31.0.0/16"}, effectiveCIDRs())
	// Audit row written in the same transaction.
	assert.EqualValues(t, 1, countRows(t, db, "audit_logs"))
	var audit model.AuditLog
	require.NoError(t, db.First(&audit).Error)
	assert.Equal(t, "system:update_ssrf_allowlist", audit.Action)
	assert.Equal(t, "admin-op", audit.Username)
	assert.Contains(t, string(audit.Detail), `"10.30.0.0/16"`)
	assert.Contains(t, string(audit.Detail), `"version":1`)
}

func TestSsrfAllowlist_Apply_ExplicitEmptyIsStrictOverride(t *testing.T) {
	strictSnapshot(t)
	db := setupSsrfAllowlistDB(t)
	svc := NewSsrfAllowlistService(db, nil, []string{"10.1.0.0/16"}, true)

	res, serr := svc.Apply(context.Background(), []string{}, 0, actor())
	require.Nil(t, serr)
	assert.Empty(t, res.EffectiveCIDRs)
	assert.Equal(t, SsrfSourceDB, res.Source)
	assert.Empty(t, effectiveCIDRs())
	// Stored as "[]" — presence + empty, distinct from absence.
	var row model.SsrfAllowlistConfig
	require.NoError(t, db.First(&row).Error)
	assert.Equal(t, "[]", row.CIDRsJSON)
}

func TestSsrfAllowlist_Apply_InvalidLeavesEverythingUntouched(t *testing.T) {
	strictSnapshot(t)
	db := setupSsrfAllowlistDB(t)
	svc := NewSsrfAllowlistService(db, nil, nil, false)
	// Precondition: a valid row is active.
	_, serr := svc.Apply(context.Background(), []string{"10.30.0.0/16"}, 0, actor())
	require.Nil(t, serr)

	serr = nil
	res, serr := svc.Apply(context.Background(), []string{"8.8.8.8/32"}, 1, actor())
	require.NotNil(t, serr)
	assert.Nil(t, res)
	assert.Equal(t, SsrfErrInvalid, serr.Kind)
	// Memory unchanged, row unchanged, no second audit row.
	assert.Equal(t, []string{"10.30.0.0/16"}, effectiveCIDRs())
	var row model.SsrfAllowlistConfig
	require.NoError(t, db.First(&row).Error)
	assert.Equal(t, int64(1), row.Version)
	assert.EqualValues(t, 1, countRows(t, db, "audit_logs"))
}

func TestSsrfAllowlist_Apply_VersionConflict(t *testing.T) {
	strictSnapshot(t)
	db := setupSsrfAllowlistDB(t)
	svc := NewSsrfAllowlistService(db, nil, nil, false)
	_, serr := svc.Apply(context.Background(), []string{"10.30.0.0/16"}, 0, actor())
	require.Nil(t, serr)

	// Stale expected_version.
	res, serr := svc.Apply(context.Background(), []string{"10.40.0.0/16"}, 0, actor())
	require.NotNil(t, serr)
	assert.Nil(t, res)
	assert.Equal(t, SsrfErrConflict, serr.Kind)
	require.NotNil(t, serr.CurrentVersion)
	assert.Equal(t, int64(1), *serr.CurrentVersion)
	assert.Equal(t, []string{"10.30.0.0/16"}, serr.CurrentCIDRs)
	// Memory and row stay at version 1.
	assert.Equal(t, []string{"10.30.0.0/16"}, effectiveCIDRs())

	// Row absent + non-zero expected → conflict.
	db2 := setupSsrfAllowlistDB(t)
	svc2 := NewSsrfAllowlistService(db2, nil, nil, false)
	_, serr = svc2.Apply(context.Background(), []string{"10.40.0.0/16"}, 5, actor())
	require.NotNil(t, serr)
	assert.Equal(t, SsrfErrConflict, serr.Kind)
}

func TestSsrfAllowlist_Apply_AuditFailureRollsBackConfig(t *testing.T) {
	strictSnapshot(t)
	db := setupSsrfAllowlistDB(t)
	// Make the same-transaction audit insert fail: drop audit_logs.
	require.NoError(t, db.Migrator().DropTable("audit_logs"))
	svc := NewSsrfAllowlistService(db, nil, nil, false)

	res, serr := svc.Apply(context.Background(), []string{"10.30.0.0/16"}, 0, actor())
	require.NotNil(t, serr)
	assert.Nil(t, res)
	assert.Equal(t, SsrfErrInternal, serr.Kind)
	// Transactional consistency: the config row was NOT persisted and the
	// memory policy was NOT published.
	assert.EqualValues(t, 0, countRows(t, db, "ssrf_allowlist_configs"))
	assert.Empty(t, effectiveCIDRs())
}

// ── Reset ──

func TestSsrfAllowlist_Reset_FallsBackToStatic(t *testing.T) {
	strictSnapshot(t)
	db := setupSsrfAllowlistDB(t)
	svc := NewSsrfAllowlistService(db, nil, []string{"10.1.0.0/16"}, true)
	_, serr := svc.Apply(context.Background(), []string{"10.30.0.0/16"}, 0, actor())
	require.Nil(t, serr)

	res, serr := svc.Reset(context.Background(), 1, actor())
	require.Nil(t, serr)
	assert.Equal(t, SsrfSourceEnv, res.Source)
	assert.Equal(t, int64(2), res.Version)
	assert.Equal(t, []string{"10.1.0.0/16"}, effectiveCIDRs())
	// ANTI-ABA: reset TOMBSTONES the row (removed=true, version bumped) —
	// the row is NOT deleted and the version NEVER resets.
	var row model.SsrfAllowlistConfig
	require.NoError(t, db.First(&row).Error)
	assert.True(t, row.Removed)
	assert.Equal(t, int64(2), row.Version)
	var audits []model.AuditLog
	require.NoError(t, db.Find(&audits).Error)
	require.Len(t, audits, 2)
	assert.Equal(t, "system:reset_ssrf_allowlist", audits[1].Action)
	assert.Contains(t, string(audits[1].Detail), `"removed":true`)
	assert.Contains(t, string(audits[1].Detail), `"version":2`)

	// Status keeps exposing the tombstone version: the NEXT writer (even one
	// creating a fresh override) must present it — never a reset-to-1 version.
	st, _ := svc.Status(context.Background())
	assert.Equal(t, int64(2), st.Version)
	assert.False(t, st.OverrideActive)
}

func TestSsrfAllowlist_Reset_NoOpAndConflicts(t *testing.T) {
	strictSnapshot(t)
	db := setupSsrfAllowlistDB(t)
	svc := NewSsrfAllowlistService(db, nil, nil, false)
	svc.LoadColdStart(context.Background())

	// Absent + expected 0: idempotent success, nothing audited.
	res, serr := svc.Reset(context.Background(), 0, actor())
	require.Nil(t, serr)
	assert.Equal(t, SsrfSourceStrict, res.Source)
	assert.EqualValues(t, 0, countRows(t, db, "audit_logs"))

	// Absent + non-zero expected: conflict.
	_, serr = svc.Reset(context.Background(), 4, actor())
	require.NotNil(t, serr)
	assert.Equal(t, SsrfErrConflict, serr.Kind)

	// Present + stale expected: conflict, row kept.
	insertRow(t, db, `["10.30.0.0/16"]`, 2)
	_, serr = svc.Reset(context.Background(), 1, actor())
	require.NotNil(t, serr)
	assert.Equal(t, SsrfErrConflict, serr.Kind)
	assert.EqualValues(t, 1, countRows(t, db, "ssrf_allowlist_configs"))

	// Tombstoned row + matching version: idempotent no-op, no bump/audit.
	require.NoError(t, db.Model(&model.SsrfAllowlistConfig{}).Where("id = ?", 1).
		Updates(map[string]any{"removed": true, "version": int64(3)}).Error)
	res, serr = svc.Reset(context.Background(), 3, actor())
	require.Nil(t, serr)
	assert.Equal(t, int64(3), res.Version)
	var row model.SsrfAllowlistConfig
	require.NoError(t, db.First(&row).Error)
	assert.Equal(t, int64(3), row.Version) // unchanged
}

// ── anti-ABA: stale expected_version across reset/recreate generations ──

func TestSsrfAllowlist_AntiABA_StaleExpectedAcrossReset(t *testing.T) {
	strictSnapshot(t)
	db := setupSsrfAllowlistDB(t)
	svc := NewSsrfAllowlistService(db, nil, nil, false)

	// Generation 1: apply v1.
	_, serr := svc.Apply(context.Background(), []string{"10.30.0.0/16"}, 0, actor())
	require.Nil(t, serr)

	// Reset → tombstone v2 (a physical delete would reset versions here).
	_, serr = svc.Reset(context.Background(), 1, actor())
	require.Nil(t, serr)

	// A STALE page holding generation-1 expected=1 must NOT be able to
	// overwrite the new generation — with a deleted-row scheme its write
	// would create a fresh v1 and silently win (classic ABA). (The CIDR is
	// deliberately VALID: the write must be rejected by the version CAS, not
	// by validation ordering.)
	res, serr := svc.Apply(context.Background(), []string{"10.99.0.0/16"}, 1, actor())
	require.NotNil(t, serr)
	assert.Nil(t, res)
	assert.Equal(t, SsrfErrConflict, serr.Kind)
	require.NotNil(t, serr.CurrentVersion)
	assert.Equal(t, int64(2), *serr.CurrentVersion)

	// Correct-generation write over the tombstone: expected=2 → v3.
	res, serr = svc.Apply(context.Background(), []string{"10.40.0.0/16"}, 2, actor())
	require.Nil(t, serr)
	assert.Equal(t, int64(3), res.Version)
	assert.Equal(t, []string{"10.40.0.0/16"}, effectiveCIDRs())

	// Row version monotonic across the whole chain.
	var row model.SsrfAllowlistConfig
	require.NoError(t, db.First(&row).Error)
	assert.Equal(t, int64(3), row.Version)
	assert.False(t, row.Removed)
}

func TestSsrfAllowlist_AntiABA_HighVersionResetNewWrite(t *testing.T) {
	strictSnapshot(t)
	db := setupSsrfAllowlistDB(t)
	svc := NewSsrfAllowlistService(db, nil, []string{"10.1.0.0/16"}, true)

	// Remote history reached v5 before this process ever wrote.
	insertRow(t, db, `["10.50.0.0/16"]`, 5)
	svc.LoadColdStart(context.Background())
	assert.Equal(t, []string{"10.50.0.0/16"}, effectiveCIDRs())

	// Reset at v5 → tombstone v6; new write at v6 → v7.
	_, serr := svc.Reset(context.Background(), 5, actor())
	require.Nil(t, serr)
	assert.Equal(t, []string{"10.1.0.0/16"}, effectiveCIDRs())
	res, serr := svc.Apply(context.Background(), []string{"10.60.0.0/16"}, 6, actor())
	require.Nil(t, serr)
	assert.Equal(t, int64(7), res.Version)

	// Stale writes from EVERY earlier generation must conflict (valid CIDR so
	// the version CAS — not validation — is what rejects them).
	for _, stale := range []int64{1, 5, 6} {
		_, serr := svc.Apply(context.Background(), []string{"10.77.0.0/16"}, stale, actor())
		require.NotNil(t, serr, "stale expected=%d must conflict", stale)
		assert.Equal(t, SsrfErrConflict, serr.Kind)
		_, serr = svc.Reset(context.Background(), stale, actor())
		require.NotNil(t, serr, "stale reset expected=%d must conflict", stale)
		assert.Equal(t, SsrfErrConflict, serr.Kind)
	}
	var row model.SsrfAllowlistConfig
	require.NoError(t, db.First(&row).Error)
	assert.Equal(t, int64(7), row.Version)
}

// ── publish ordering: a stale observation can never roll the policy back ──

func TestSsrfAllowlist_PublishGuard_StaleObservationRejected(t *testing.T) {
	strictSnapshot(t)
	db := setupSsrfAllowlistDB(t)
	svc := NewSsrfAllowlistService(db, nil, []string{"10.1.0.0/16"}, true)

	// Progress to v3 (v1 apply, external bump to v3, refresh publishes it).
	_, serr := svc.Apply(context.Background(), []string{"10.30.0.0/16"}, 0, actor())
	require.Nil(t, serr)
	require.NoError(t, db.Model(&model.SsrfAllowlistConfig{}).Where("id = ?", 1).
		Updates(map[string]any{"cidrs": `["10.60.0.0/16"]`, "version": int64(3)}).Error)
	require.NoError(t, svc.Refresh(context.Background()))
	require.Equal(t, []string{"10.60.0.0/16"}, effectiveCIDRs())

	// A SLOW refresh that observed the v2-era row (simulated deterministically
	// by calling the guarded publish with the older observation) must be
	// REFUSED — not merely repaired later by the TTL.
	assert.False(t, svc.publishDBGuarded([]string{"10.70.0.0/16"}, 2), "stale db publish must be refused")
	assert.Equal(t, []string{"10.60.0.0/16"}, effectiveCIDRs())

	// Same for a stale static publish (tombstone observed at v2).
	assert.False(t, svc.publishStaticGuarded(2), "stale static publish must be refused")
	assert.Equal(t, []string{"10.60.0.0/16"}, effectiveCIDRs())

	// Same-version replay is refused too (idempotence, not duplicate work).
	assert.False(t, svc.publishDBGuarded([]string{"10.60.0.0/16"}, 3))

	// A strictly newer observation still publishes.
	assert.True(t, svc.publishDBGuarded([]string{"10.80.0.0/16"}, 4))
	assert.Equal(t, []string{"10.80.0.0/16"}, effectiveCIDRs())
}

func TestSsrfAllowlist_PublishGuard_ConcurrentWritersEndConsistent(t *testing.T) {
	strictSnapshot(t)
	db := setupSsrfAllowlistDB(t)
	svc := NewSsrfAllowlistService(db, nil, []string{"10.1.0.0/16"}, true)

	// Concurrent writers race on the CAS; every commit publishes through the
	// monotonic guard. After the storm, memory MUST equal the committed row
	// (no stale publish survived) and versions must be gap-free.
	const writers, rounds = 4, 12
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				st, _ := svc.Status(context.Background())
				if r%2 == 0 {
					_, _ = svc.Apply(context.Background(), []string{fmt.Sprintf("10.%d.0.0/16", 100+w)}, st.Version, actor())
				} else {
					_, _ = svc.Reset(context.Background(), st.Version, actor())
				}
			}
		}(w)
	}
	wg.Wait()

	// Force a final reconciliation from the committed row and assert the
	// invariant: the published policy IS the row's policy (or the static
	// fallback for a tombstone), never an older generation.
	require.NoError(t, svc.Refresh(context.Background()))
	var row model.SsrfAllowlistConfig
	require.NoError(t, db.First(&row).Error)
	if row.Removed {
		assert.Equal(t, []string{"10.1.0.0/16"}, effectiveCIDRs())
	} else {
		want, err := parseCIDRsJSON(row.CIDRsJSON)
		require.NoError(t, err)
		assert.Equal(t, want, effectiveCIDRs())
	}
	// Commits are serialized by the CAS: one row, version = 1+successful ops.
	assert.EqualValues(t, 1, countRows(t, db, "ssrf_allowlist_configs"))
	assert.LessOrEqual(t, row.Version, int64(1+writers*rounds))
}

// ── Refresh: runtime failures keep last-known-good; external changes apply ──

func TestSsrfAllowlist_Refresh_ExternalChangeAndDelete(t *testing.T) {
	strictSnapshot(t)
	db := setupSsrfAllowlistDB(t)
	svc := NewSsrfAllowlistService(db, nil, []string{"10.1.0.0/16"}, true)
	svc.LoadColdStart(context.Background())
	assert.Equal(t, []string{"10.1.0.0/16"}, effectiveCIDRs())

	// Another instance writes version 1.
	insertRow(t, db, `["10.50.0.0/16"]`, 1)
	require.NoError(t, svc.Refresh(context.Background()))
	assert.Equal(t, []string{"10.50.0.0/16"}, effectiveCIDRs())

	// Another instance bumps to version 2.
	require.NoError(t, db.Model(&model.SsrfAllowlistConfig{}).Where("id = ?", 1).
		Updates(map[string]any{"cidrs": `["10.60.0.0/16"]`, "version": int64(2)}).Error)
	require.NoError(t, svc.Refresh(context.Background()))
	assert.Equal(t, []string{"10.60.0.0/16"}, effectiveCIDRs())

	// Another instance tombstones the override (bumping the version, as a
	// real reset does) → fall back to static.
	require.NoError(t, db.Model(&model.SsrfAllowlistConfig{}).Where("id = ?", 1).
		Updates(map[string]any{"removed": true, "version": int64(3), "cidrs": "[]"}).Error)
	require.NoError(t, svc.Refresh(context.Background()))
	assert.Equal(t, []string{"10.1.0.0/16"}, effectiveCIDRs())
	st, _ := svc.Status(context.Background())
	assert.Equal(t, SsrfSourceEnv, st.Source)
	assert.Equal(t, int64(3), st.Version)
}

func TestSsrfAllowlist_Refresh_FailuresKeepLastKnownGood(t *testing.T) {
	strictSnapshot(t)

	t.Run("invalid row keeps last-known-good + degraded", func(t *testing.T) {
		db := setupSsrfAllowlistDB(t)
		svc := NewSsrfAllowlistService(db, nil, nil, false)
		_, serr := svc.Apply(context.Background(), []string{"10.30.0.0/16"}, 0, actor())
		require.Nil(t, serr)

		require.NoError(t, db.Model(&model.SsrfAllowlistConfig{}).Where("id = ?", 1).
			Update("cidrs", `["8.8.8.8/32"]`).Error)
		err := svc.Refresh(context.Background())
		require.Error(t, err)
		assert.Equal(t, []string{"10.30.0.0/16"}, effectiveCIDRs()) // last-known-good
		st, _ := svc.Status(context.Background())
		assert.Contains(t, st.Degraded, "last-known-good")
	})

	t.Run("db read failure keeps last-known-good", func(t *testing.T) {
		db := setupSsrfAllowlistDB(t)
		svc := NewSsrfAllowlistService(db, nil, nil, false)
		_, serr := svc.Apply(context.Background(), []string{"10.30.0.0/16"}, 0, actor())
		require.Nil(t, serr)

		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
		require.Error(t, svc.Refresh(context.Background()))
		assert.Equal(t, []string{"10.30.0.0/16"}, effectiveCIDRs())
		st, _ := svc.Status(context.Background())
		assert.Contains(t, st.Degraded, "last-known-good")
	})
}

// ── TTL convergence (bounded staleness, no Redis) ──

func TestSsrfAllowlist_TTLRefresh_ConvergesWithoutRedis(t *testing.T) {
	strictSnapshot(t)
	db := setupSsrfAllowlistDB(t)
	prev := ssrfAllowlistRefreshTTL
	ssrfAllowlistRefreshTTL = 50 * time.Millisecond
	t.Cleanup(func() { ssrfAllowlistRefreshTTL = prev })

	svcA := NewSsrfAllowlistService(db, nil, nil, false)
	svcA.LoadColdStart(context.Background())
	svcB := NewSsrfAllowlistService(db, nil, nil, false)
	svcB.LoadColdStart(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svcB.Start(ctx)

	// Instance A applies a change (no Redis: instance B must converge via TTL).
	// NOTE: within one process the guardrail policy is a package global, so
	// "effective memory" cannot distinguish instances — the assertion is on
	// instance B's OWN state (source+version), which only advances when B's
	// refresh loop actually re-read the row.
	_, serr := svcA.Apply(context.Background(), []string{"10.70.0.0/16"}, 0, actor())
	require.Nil(t, serr)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := svcB.Status(context.Background()); st.Source == SsrfSourceDB && st.Version == 1 {
			return // converged via TTL pull
		}
		time.Sleep(20 * time.Millisecond)
	}
	stB, _ := svcB.Status(context.Background())
	t.Fatalf("instance B did not converge via TTL within 3s: source=%s version=%d", stB.Source, stB.Version)
}

func effectiveCIDRsB(t *testing.T, svc *SsrfAllowlistService) []string {
	t.Helper()
	st, _ := svc.Status(context.Background())
	return st.EffectiveCIDRs
}

// waitAllowlistSubscribed blocks until the redis client observes ≥1
// subscriber on the allowlist channel, so the test's first Apply cannot
// PUBLISH before the subscriber registered — the race that made the
// broadcast tests flaky under full-tree parallel load (Start now also
// subscribes synchronously; this barrier makes the intent explicit).
func waitAllowlistSubscribed(t *testing.T, rdb *redis.Client) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n, err := rdb.PubSubNumSub(context.Background(), SsrfAllowlistChannel).Result(); err == nil && n[SsrfAllowlistChannel] >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("allowlist subscriber did not register within 2s")
}

// ── multi-instance: versioned Redis invalidation broadcast ──

func TestSsrfAllowlist_RedisBroadcast_SecondInstanceConverges(t *testing.T) {
	strictSnapshot(t)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	db := setupSsrfAllowlistDB(t)
	svcA := NewSsrfAllowlistService(db, rdb, nil, false)
	svcB := NewSsrfAllowlistService(db, rdb, nil, false)
	svcA.LoadColdStart(context.Background())
	svcB.LoadColdStart(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svcB.Start(ctx)
	waitAllowlistSubscribed(t, rdb)

	// A applies → broadcast → B refreshes well before any TTL.
	prev := ssrfAllowlistRefreshTTL
	ssrfAllowlistRefreshTTL = time.Hour // rule out the TTL path
	t.Cleanup(func() { ssrfAllowlistRefreshTTL = prev })

	res, serr := svcA.Apply(context.Background(), []string{"10.80.0.0/16"}, 0, actor())
	require.Nil(t, serr)
	assert.Equal(t, "redis", res.Broadcast)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := svcB.Status(context.Background()); st.Source == SsrfSourceDB && st.Version == res.Version {
			return // converged via broadcast
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("second instance did not converge via Redis broadcast within 3s")
}

func TestSsrfAllowlist_ResetBroadcast_SecondInstanceFallsBack(t *testing.T) {
	strictSnapshot(t)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	db := setupSsrfAllowlistDB(t)
	svcA := NewSsrfAllowlistService(db, rdb, []string{"10.1.0.0/16"}, true)
	svcB := NewSsrfAllowlistService(db, rdb, []string{"10.1.0.0/16"}, true)
	svcA.LoadColdStart(context.Background())
	svcB.LoadColdStart(context.Background())
	_, serr := svcA.Apply(context.Background(), []string{"10.80.0.0/16"}, 0, actor())
	require.Nil(t, serr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svcB.Start(ctx)
	waitAllowlistSubscribed(t, rdb)

	prev := ssrfAllowlistRefreshTTL
	ssrfAllowlistRefreshTTL = time.Hour
	t.Cleanup(func() { ssrfAllowlistRefreshTTL = prev })

	_, serr = svcA.Reset(context.Background(), 1, actor())
	require.Nil(t, serr)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := svcB.Status(context.Background()); st.Source == SsrfSourceEnv {
			return // broadcast with version 0 was NOT deduped away
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("second instance did not fall back after reset broadcast within 3s")
}

// ── Status shape ──

func TestSsrfAllowlist_Status_FallbackPreviewAndConsistency(t *testing.T) {
	strictSnapshot(t)
	db := setupSsrfAllowlistDB(t)
	svc := NewSsrfAllowlistService(db, nil, []string{"10.1.0.0/16"}, true)
	_, serr := svc.Apply(context.Background(), []string{"10.30.0.0/16"}, 0, actor())
	require.Nil(t, serr)

	st, err := svc.Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"10.30.0.0/16"}, st.EffectiveCIDRs)
	assert.Equal(t, SsrfSourceDB, st.Source)
	assert.Equal(t, int64(1), st.Version)
	assert.Equal(t, []string{"10.1.0.0/16"}, st.FallbackCIDRs)
	assert.Equal(t, SsrfSourceEnv, st.FallbackSource)
	assert.Equal(t, "ttl-only", st.Consistency.Broadcast)
	assert.Equal(t, 30, st.Consistency.WindowSeconds)
}
