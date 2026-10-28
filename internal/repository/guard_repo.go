package repository

import (
	"context"
	"time"

	"github.com/crosslink/internal/model"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// GuardTokenRepo 存取旁路检测接入凭证。管理操作全部以 org 为强制参数——
// 一个 token 只属于一个 org，跨 org 操作无合法场景。
type GuardTokenRepo struct {
	db *gorm.DB
}

func NewGuardTokenRepo(db *gorm.DB) *GuardTokenRepo {
	return &GuardTokenRepo{db: db}
}

func (r *GuardTokenRepo) Create(ctx context.Context, t *model.GuardIngestToken) error {
	return r.db.WithContext(ctx).Create(t).Error
}

// FindByHash 供 ingest 认证中间件做 Bearer → org 映射（含吊销状态检查）。
func (r *GuardTokenRepo) FindByHash(ctx context.Context, hash string) (*model.GuardIngestToken, error) {
	var tok model.GuardIngestToken
	if err := r.db.WithContext(ctx).Where("token_hash = ?", hash).First(&tok).Error; err != nil {
		return nil, err
	}
	return &tok, nil
}

func (r *GuardTokenRepo) ListByOrg(ctx context.Context, orgID int64) ([]model.GuardIngestToken, error) {
	var toks []model.GuardIngestToken
	err := r.db.WithContext(ctx).Where("org_id = ?", orgID).Order("created_at DESC").Find(&toks).Error
	return toks, err
}

// Revoke 吊销（org 范围内按 ID；幂等：已吊销再吊销无副作用）。
func (r *GuardTokenRepo) Revoke(ctx context.Context, orgID, id int64) error {
	return r.db.WithContext(ctx).Model(&model.GuardIngestToken{}).
		Where("id = ? AND org_id = ?", id, orgID).
		Updates(map[string]interface{}{"status": 0, "revoked_at": time.Now()}).Error
}

// TouchLastUsed 更新最近使用时间，节流为每 token 至多 60s 一次
// （pat_token 同模式，避免镜像高频流量打满写入）。
func (r *GuardTokenRepo) TouchLastUsed(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Model(&model.GuardIngestToken{}).
		Where("id = ? AND (last_used_at IS NULL OR last_used_at < ?)", id, time.Now().Add(-60*time.Second)).
		Update("last_used_at", time.Now()).Error
}

// UpdateCredential 原地换发（Reset 语义）：行 ID 不变（外部引用与审计连
// 续），哈希与前缀更新，吊销状态清除。org 范围内按 ID。
func (r *GuardTokenRepo) UpdateCredential(ctx context.Context, orgID, id int64, newHash, newPrefix string) (*model.GuardIngestToken, error) {
	var tok model.GuardIngestToken
	if err := r.db.WithContext(ctx).Model(&model.GuardIngestToken{}).
		Where("id = ? AND org_id = ?", id, orgID).
		Updates(map[string]interface{}{
			"token_hash": newHash, "key_prefix": newPrefix,
			"status": 1, "revoked_at": nil,
		}).Error; err != nil {
		return nil, err
	}
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&tok).Error; err != nil {
		return nil, err
	}
	return &tok, nil
}

// HashByID 取当前哈希（凭证吊销/重置后的缓存失效用）；查不到返回空串。
func (r *GuardTokenRepo) HashByID(ctx context.Context, orgID, id int64) string {
	var hashes []string
	r.db.WithContext(ctx).Model(&model.GuardIngestToken{}).
		Where("id = ? AND org_id = ?", id, orgID).
		Pluck("token_hash", &hashes)
	if len(hashes) > 0 {
		return hashes[0]
	}
	return ""
}

// GuardEventRepo 旁路检测事件留存。ListByOrg 强制 org 过滤——多租户共享
// 部署下不存在无 org 上下文的事件查询。
type GuardEventRepo struct {
	db *gorm.DB
}

func NewGuardEventRepo(db *gorm.DB) *GuardEventRepo {
	return &GuardEventRepo{db: db}
}

// Create 落库一条检测事件；Metadata 为 nil 时写空对象（列 NOT NULL）。
func (r *GuardEventRepo) Create(ctx context.Context, e *model.GuardEvent) error {
	if len(e.Metadata) == 0 {
		e.Metadata = datatypes.JSON([]byte("{}"))
	}
	return r.db.WithContext(ctx).Create(e).Error
}

func (r *GuardEventRepo) ListByOrg(ctx context.Context, orgID int64, limit, offset int) ([]model.GuardEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var events []model.GuardEvent
	err := r.db.WithContext(ctx).Where("org_id = ?", orgID).
		Order("created_at DESC").Limit(limit).Offset(offset).Find(&events).Error
	return events, err
}

func (r *GuardEventRepo) ListByTrace(ctx context.Context, orgID int64, traceID string) ([]model.GuardEvent, error) {
	var events []model.GuardEvent
	err := r.db.WithContext(ctx).Where("org_id = ? AND trace_id = ?", orgID, traceID).
		Order("created_at ASC").Find(&events).Error
	return events, err
}

// DeleteOlderThan 留存清理（平台级后台任务，无 org 维度）。
func (r *GuardEventRepo) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Where("created_at < ?", cutoff).Delete(&model.GuardEvent{})
	return res.RowsAffected, res.Error
}

// GuardTraceRepo trace 元数据存取。配对竞争采用 last-write-wins：元数据
// 仅作观测汇总，事件本身在 guard_events 有独立留存。
type GuardTraceRepo struct {
	db *gorm.DB
}

func NewGuardTraceRepo(db *gorm.DB) *GuardTraceRepo {
	return &GuardTraceRepo{db: db}
}

func (r *GuardTraceRepo) FindByOrgTrace(ctx context.Context, orgID int64, traceID string) (*model.GuardTrace, error) {
	var tr model.GuardTrace
	if err := r.db.WithContext(ctx).Where("org_id = ? AND trace_id = ?", orgID, traceID).First(&tr).Error; err != nil {
		return nil, err
	}
	return &tr, nil
}

func (r *GuardTraceRepo) Create(ctx context.Context, tr *model.GuardTrace) error {
	return r.db.WithContext(ctx).Create(tr).Error
}

func (r *GuardTraceRepo) Save(ctx context.Context, tr *model.GuardTrace) error {
	return r.db.WithContext(ctx).Save(tr).Error
}

func (r *GuardTraceRepo) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Where("created_at < ?", cutoff).Delete(&model.GuardTrace{})
	return res.RowsAffected, res.Error
}
