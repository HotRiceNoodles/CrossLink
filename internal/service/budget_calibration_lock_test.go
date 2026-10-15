package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/crosslink/internal/model"
	sqlite "github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Cross-replica calibration-lock regression suite (supervisor review): the
// lock must actually be a lock — manual/scheduled entry points share it,
// store failures SKIP (never run unlocked), and a lease that expires mid-run
// is rejected at write time by ownership check.

func newCalibrationFixture(t *testing.T) (*BudgetCalibrationService, *BudgetCalibrationService, *miniredis.Miniredis, *gorm.DB) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Team{}, &model.Organization{}, &model.APIKey{}, &model.UsageLog{}, &model.BudgetSnapshot{}))
	// One team with a budget forces at least one calibrated Redis write,
	// exercising the ownership-checked script on every round.
	require.NoError(t, db.Create(&model.Team{Name: "t1", BudgetLimit: 100, BudgetPeriod: "monthly"}).Error)
	a := NewBudgetCalibrationService(db, rdb)
	b := NewBudgetCalibrationService(db, rdb)
	return a, b, mr, db
}

// Two replicas racing (one manual trigger, one scheduled round): exactly one
// wins the lock; the other gets a RECOGNIZABLE held error — never a silent
// unlocked run.
func TestCalibrationLock_TwoReplicasManualVsScheduled(t *testing.T) {
	a, b, mr, _ := newCalibrationFixture(t)
	ctx := context.Background()

	// Deterministic contention: a grabs the lock first and holds it.
	token, release, err := a.acquireCalibrationLock(ctx)
	require.NoError(t, err)
	require.NotNil(t, release)
	require.NotEmpty(t, token)
	require.True(t, mr.Exists(calibrationLockKey))

	// The scheduled round on the other replica must skip.
	bErr := b.CalibrateOnce(ctx)
	require.True(t, errors.Is(bErr, ErrCalibrationLockHeld), "want ErrCalibrationLockHeld, got %v", bErr)

	release()
	// After release the other replica's MANUAL trigger succeeds end to end.
	require.NoError(t, b.CalibrateOnce(ctx))
}

// Two concurrent manual triggers from two replicas: exactly one success.
func TestCalibrationLock_TwoReplicasConcurrentManual(t *testing.T) {
	a, b, _, _ := newCalibrationFixture(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, svc := range []*BudgetCalibrationService{a, b} {
		wg.Add(1)
		go func(i int, svc *BudgetCalibrationService) {
			defer wg.Done()
			errs[i] = svc.CalibrateOnce(ctx)
		}(i, svc)
	}
	wg.Wait()
	held := 0
	for _, e := range errs {
		if errors.Is(e, ErrCalibrationLockHeld) {
			held++
		} else {
			require.NoError(t, e)
		}
	}
	require.Equal(t, 1, held, "exactly one replica must lose the lock race (errs=%v)", errs)
}

// Redis (lock store) failure: the round must SKIP with a recognizable
// error — the old behavior of proceeding unlocked is gone.
func TestCalibrationLock_StoreFailureSkipsNeverUnlocked(t *testing.T) {
	a, _, mr, _ := newCalibrationFixture(t)
	ctx := context.Background()
	mr.Close() // store down

	err := a.CalibrateOnce(ctx)
	require.True(t, errors.Is(err, ErrCalibrationLockUnavailable), "want ErrCalibrationLockUnavailable, got %v", err)
	// The scheduled path handles the same error without panicking.
	a.calibrateAll(ctx)
}

// Lease expiry: after TTL the lock is free for a new holder, and the STALE
// holder's writes are rejected by the ownership check inside the calibrated
// write (write-time ownership, not just acquisition-time).
func TestCalibrationLock_LeaseExpiryAndStaleHolderWriteRejected(t *testing.T) {
	a, b, mr, db := newCalibrationFixture(t)
	ctx := context.Background()

	staleToken, release, err := a.acquireCalibrationLock(ctx)
	require.NoError(t, err)
	require.NotNil(t, release)

	// Expire the lease out from under the stale holder.
	mr.FastForward(calibrationLockTTL + time.Minute)
	require.False(t, mr.Exists(calibrationLockKey), "lease should have expired")

	// A new holder may now acquire…
	newToken, newRelease, err := b.acquireCalibrationLock(ctx)
	require.NoError(t, err)
	require.NotNil(t, newRelease)
	require.NotEqual(t, staleToken, newToken)

	// …and the stale holder's calibrated write is REJECTED (res == -1 path).
	lerr := a.syncRedisAndSnapshot(ctx, staleToken, "team", 1, PeriodKey("monthly"), "monthly", 42.0, 100)
	require.True(t, errors.Is(lerr, ErrCalibrationLockLost), "stale holder write must be rejected, got %v", lerr)

	// The budget counter must be untouched by the rejected write.
	val, err := a.rdb.Get(ctx, "budget:team:1:"+PeriodKey("monthly")).Result()
	require.True(t, errors.Is(err, redis.Nil), "rejected write must not create the counter, got %q", val)

	// The new holder's write goes through.
	require.NoError(t, b.syncRedisAndSnapshot(ctx, newToken, "team", 1, PeriodKey("monthly"), "monthly", 42.0, 100))
	got, err := a.rdb.Get(ctx, "budget:team:1:"+PeriodKey("monthly")).Float64()
	require.NoError(t, err)
	require.Equal(t, 42.0, got)

	// Snapshot rows only from the accepted write.
	var snaps []model.BudgetSnapshot
	require.NoError(t, db.Find(&snaps).Error)
	require.Len(t, snaps, 1)

	newRelease()
	release() // stale token: unlock script is a no-op (ownership mismatch)
}
