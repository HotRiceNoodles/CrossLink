package behavior

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/crosslink/internal/model"
)

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "behavior.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.UsageLog{}, &model.BehaviorRollup{}, &model.BehaviorBaseline{},
		&model.BehaviorJobLock{}, &model.BehaviorSeenCategory{},
		&model.BehaviorAnomaly{}, &model.BehaviorTolerance{}, &model.BehaviorFingerprint{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close() // Windows 上不关闭会锁住 TempDir 导致清理失败
		}
	})
	return db
}

func TestJobLockAcquireRenewRelease(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	_, ok, err := AcquireJobLock(ctx, db, JobHourly, "ownerA", time.Minute)
	if err != nil || !ok {
		t.Fatalf("first acquire failed: ok=%v err=%v", ok, err)
	}
	// 他人竞争失败。
	_, ok, err = AcquireJobLock(ctx, db, JobHourly, "ownerB", time.Minute)
	if err != nil {
		t.Fatalf("competing acquire errored: %v", err)
	}
	if ok {
		t.Fatal("ownerB must not acquire a held unexpired lease")
	}
	// 持有者续租成功。
	if ok, err := RenewJobLock(ctx, db, JobHourly, "ownerA", time.Minute); err != nil || !ok {
		t.Fatalf("renew by owner failed: ok=%v err=%v", ok, err)
	}
	// 非持有者续租失败（D15：被抢占者续不了租）。
	if ok, _ := RenewJobLock(ctx, db, JobHourly, "ownerB", time.Minute); ok {
		t.Fatal("renew by non-owner must fail")
	}
	// OwnCheck。
	if ok, _ := OwnCheck(ctx, db, JobHourly, "ownerA"); !ok {
		t.Fatal("own check should pass for holder")
	}
	if err := ReleaseJobLock(ctx, db, JobHourly, "ownerA"); err != nil {
		t.Fatalf("release: %v", err)
	}
	// 释放后他人可获得——行已删除，走 INSERT 路径 fence=1（fence 递增仅在
	// 过期接管路径，见 TestJobLockExpiredTakeoverAndConditionalRelease）。
	fence, ok, err := AcquireJobLock(ctx, db, JobHourly, "ownerB", time.Minute)
	if err != nil || !ok {
		t.Fatalf("acquire after release failed: ok=%v err=%v", ok, err)
	}
	if fence != 1 {
		t.Fatalf("fence = %d, want 1 (fresh row after release)", fence)
	}
}

func TestJobLockExpiredTakeoverAndConditionalRelease(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	lease := 50 * time.Millisecond

	_, ok, err := AcquireJobLock(ctx, db, JobDaily, "ownerA", lease)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	time.Sleep(80 * time.Millisecond) // 租约过期

	// 过期后他人原子接管（fence=2）。
	fence, ok, err := AcquireJobLock(ctx, db, JobDaily, "ownerB", lease)
	if err != nil || !ok {
		t.Fatalf("takeover failed: ok=%v err=%v", ok, err)
	}
	if fence != 2 {
		t.Fatalf("fence after takeover = %d, want 2", fence)
	}
	// 旧 owner 的条件释放不能删掉新 owner 的锁（三轮评审 ⑤ 的核心场景）。
	if err := ReleaseJobLock(ctx, db, JobDaily, "ownerA"); err != nil {
		t.Fatalf("stale release errored: %v", err)
	}
	if ok, _ := OwnCheck(ctx, db, JobDaily, "ownerB"); !ok {
		t.Fatal("ownerB's lease must survive ownerA's stale release")
	}
}
