package behavior

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"gorm.io/gorm"

	"github.com/crosslink/internal/model"
)

// 可移植作业互斥（规划冻结 v4.0 D15）。语义：
//
//   - 抢锁两步（每步单语句原子）：① UPDATE 已过期的租约（fence=fence+1），命中即获得；
//     ② INSERT 新行，撞唯一键即他人持有，本轮放弃。跨实例竞态必然收敛于其中一步。
//   - 续租/释放都带 owner_token 条件——被抢占的旧 owner 续不了租、也删不掉后继者的锁。
//   - fence 单调递增，仅代际记录与诊断；不参与写入校验（明示取舍，规划 §5.3 的
//     逐写类检测/恢复表承担正确性）。
//
// SQLite 单写者天然串行化；MySQL/PG 依赖语句级原子性与行锁。

// NewOwnerToken 生成随机的持有者令牌。
func NewOwnerToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 降级为纳秒时间戳——唯一性足够（单进程内并发调用是唯一冲突源）。
		return time.Now().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b)
}

// AcquireJobLock 尝试获取 job 的租约。返回 (fence, true, nil) 表示成功；
// ok=false 表示他人持有（正常竞争，不是错误）。
func AcquireJobLock(ctx context.Context, db *gorm.DB, job, owner string, lease time.Duration) (int64, bool, error) {
	now := time.Now()
	// 第 1 步：抢占过期租约（原子 UPDATE，fence 单调递增）。
	res := db.WithContext(ctx).
		Model(&model.BehaviorJobLock{}).
		Where("job = ? AND lease_until < ?", job, now).
		Updates(map[string]any{
			"owner_token": owner,
			"fence":       gorm.Expr("fence + 1"),
			"lease_until": now.Add(lease),
		})
	if res.Error != nil {
		return 0, false, res.Error
	}
	if res.RowsAffected == 1 {
		return readFence(ctx, db, job, owner)
	}
	// 第 2 步：无过期租约则尝试新建。失败时回查区分“他人持有”与真实错误。
	row := &model.BehaviorJobLock{Job: job, OwnerToken: owner, Fence: 1, LeaseUntil: now.Add(lease)}
	if err := db.WithContext(ctx).Create(row).Error; err != nil {
		var existing model.BehaviorJobLock
		if qErr := db.WithContext(ctx).Where("job = ?", job).First(&existing).Error; qErr == nil && existing.OwnerToken != owner {
			return 0, false, nil // 他人持有——正常竞争
		}
		return 0, false, err // 真实错误（或回查也失败）
	}
	return 1, true, nil
}

// readFence 读回自己刚抢到的租约代际（UPDATE 无法返回新值，MySQL 无 RETURNING）。
// 竞态安全：owner_token 归自己之后，只有自己的 Release 会改行。
func readFence(ctx context.Context, db *gorm.DB, job, owner string) (int64, bool, error) {
	var row model.BehaviorJobLock
	if err := db.WithContext(ctx).
		Where("job = ? AND owner_token = ?", job, owner).
		First(&row).Error; err != nil {
		return 0, false, err
	}
	return row.Fence, true, nil
}

// RenewJobLock 续租（仅自己的租约）。返回 false=已易主，调用方必须立即中止本轮作业。
func RenewJobLock(ctx context.Context, db *gorm.DB, job, owner string, lease time.Duration) (bool, error) {
	res := db.WithContext(ctx).
		Model(&model.BehaviorJobLock{}).
		Where("job = ? AND owner_token = ?", job, owner).
		Update("lease_until", time.Now().Add(lease))
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// ReleaseJobLock 条件释放：只删自己的租约，不可能删掉后继者的锁。
func ReleaseJobLock(ctx context.Context, db *gorm.DB, job, owner string) error {
	return db.WithContext(ctx).
		Where("job = ? AND owner_token = ?", job, owner).
		Delete(&model.BehaviorJobLock{}).Error
}

// OwnCheck 确认当前仍持有租约（台账写入前的窄窗口自检，§5.3）。
func OwnCheck(ctx context.Context, db *gorm.DB, job, owner string) (bool, error) {
	var n int64
	if err := db.WithContext(ctx).
		Model(&model.BehaviorJobLock{}).
		Where("job = ? AND owner_token = ?", job, owner).
		Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}
