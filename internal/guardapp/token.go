package guardapp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
)

// ingestTokenPrefix 标识 crosslink-guard 接入凭证，与网关 sk- 键空间区分。
const ingestTokenPrefix = "cg-"

// HashToken 是 ingest 凭证的存库形态（SHA-256 hex）——认证与签发共用。
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// CreatedToken 是签发结果：Token 明文仅在创建/重置响应中出现一次，库中
// 只有哈希与前缀。
type CreatedToken struct {
	ID        int64     `json:"id"`
	OrgID     int64     `json:"org_id"`
	Name      string    `json:"name"`
	Token     string    `json:"token"` // 明文，仅此一次
	KeyPrefix string    `json:"key_prefix"`
	CreatedAt time.Time `json:"created_at"`
}

// IngestTokenService 管理 ingest 凭证生命周期。所有变更操作以 org 为强制
// 参数（多租户共享部署下跨 org 操作无合法场景）。
type IngestTokenService struct {
	repo  *repository.GuardTokenRepo
	cache *tokenCache
}

func NewIngestTokenService(repo *repository.GuardTokenRepo, cache *tokenCache) *IngestTokenService {
	return &IngestTokenService{repo: repo, cache: cache}
}

// newTokenSecret 生成 "cg-" + 24 随机字节的 hex 明文。
func newTokenSecret() (plain string, err error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return ingestTokenPrefix + hex.EncodeToString(b), nil
}

// Create 为 org 签发新凭证。name 是运维备注（如 "edge-mirror"）。
func (s *IngestTokenService) Create(ctx context.Context, orgID int64, name string) (*CreatedToken, error) {
	plain, err := newTokenSecret()
	if err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}
	tok := &model.GuardIngestToken{
		OrgID:     orgID,
		Name:      name,
		TokenHash: HashToken(plain),
		KeyPrefix: plain[:len(ingestTokenPrefix)+8],
		HashAlgo:  "sha256",
		Status:    1,
	}
	if err := s.repo.Create(ctx, tok); err != nil {
		return nil, err
	}
	return &CreatedToken{
		ID: tok.ID, OrgID: tok.OrgID, Name: tok.Name,
		Token: plain, KeyPrefix: tok.KeyPrefix, CreatedAt: tok.CreatedAt,
	}, nil
}

// Revoke 吊销（幂等）；同节点立即清缓存，多节点最长 cache TTL 后生效。
func (s *IngestTokenService) Revoke(ctx context.Context, orgID, id int64) error {
	if err := s.repo.Revoke(ctx, orgID, id); err != nil {
		return err
	}
	s.invalidate(ctx, orgID, id)
	return nil
}

// Reset 换发明文：token 行保留（外部引用的 ID 不变、审计连续），哈希与
// 前缀原地更新，旧明文即刻作废。
func (s *IngestTokenService) Reset(ctx context.Context, orgID, id int64) (*CreatedToken, error) {
	plain, err := newTokenSecret()
	if err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}
	tok, err := s.repo.UpdateCredential(ctx, orgID, id, HashToken(plain), plain[:len(ingestTokenPrefix)+8])
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx, orgID, id)
	return &CreatedToken{
		ID: tok.ID, OrgID: tok.OrgID, Name: tok.Name,
		Token: plain, KeyPrefix: tok.KeyPrefix, CreatedAt: tok.CreatedAt,
	}, nil
}

// invalidate 先取行拿旧哈希再清缓存；删除失败只影响本节点即时性（TTL 兜
// 底），不作为错误上抛。
func (s *IngestTokenService) invalidate(ctx context.Context, orgID, id int64) {
	hash := s.repo.HashByID(ctx, orgID, id)
	if hash != "" {
		s.cache.delete(hash)
	}
}
