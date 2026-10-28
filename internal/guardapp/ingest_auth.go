package guardapp

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/gin-gonic/gin"
)

// ingestCacheTTL 是 Bearer→凭证行的正缓存窗口，也是吊销/重置在多节点部署
// 下的生效上界（单节点由 invalidate 即时清除）。
const ingestCacheTTL = 30 * time.Second

type cacheEntry struct {
	tok *model.GuardIngestToken
	exp time.Time
}

// tokenCache 只做正缓存；无效凭证不缓存（每次落库比对，纯 SHA-256 无
// bcrypt 成本；防扫描由 Step 7 的两级限速负责）。
type tokenCache struct {
	mu  sync.RWMutex
	m   map[string]cacheEntry
	ttl time.Duration
	now func() time.Time // 可注入时钟，测试 TTL 行为
}

func newTokenCache() *tokenCache {
	return &tokenCache{m: make(map[string]cacheEntry), ttl: ingestCacheTTL, now: time.Now}
}

func (c *tokenCache) get(hash string) (*model.GuardIngestToken, bool) {
	c.mu.RLock()
	e, ok := c.m[hash]
	c.mu.RUnlock()
	if !ok || c.now().After(e.exp) {
		return nil, false
	}
	return e.tok, true
}

func (c *tokenCache) put(tok *model.GuardIngestToken) {
	c.mu.Lock()
	c.m[tok.TokenHash] = cacheEntry{tok: tok, exp: c.now().Add(c.ttl)}
	c.mu.Unlock()
}

func (c *tokenCache) delete(hash string) {
	c.mu.Lock()
	delete(c.m, hash)
	c.mu.Unlock()
}

// IngestAuth 解析 Authorization: Bearer <明文>，完成凭证→org 映射并将
// org_id / guard_token_id 写入请求上下文。无效或已吊销凭证一律 401。
func IngestAuth(repo *repository.GuardTokenRepo, cache *tokenCache) gin.HandlerFunc {
	return func(c *gin.Context) {
		plain := bearerToken(c.GetHeader("Authorization"))
		if plain == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{"type": "invalid_ingest_token", "message": "missing bearer token"},
			})
			return
		}
		hash := HashToken(plain)
		tok, ok := cache.get(hash)
		if !ok {
			var err error
			tok, err = repo.FindByHash(c.Request.Context(), hash)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
					"error": gin.H{"type": "invalid_ingest_token", "message": "invalid bearer token"},
				})
				return
			}
			cache.put(tok)
		}
		if tok.Status != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{"type": "invalid_ingest_token", "message": "token revoked"},
			})
			return
		}
		c.Set("org_id", tok.OrgID)
		c.Set("guard_token_id", tok.ID)
		c.Set("guard_token_name", tok.Name)
		// fire-and-forget：60s 节流在 SQL 条件里，高频镜像流量下每 token
		// 至多每分钟一次 UPDATE。
		_ = repo.TouchLastUsed(c.Request.Context(), tok.ID)
		c.Next()
	}
}

// RequirePayloadOrg 校验 payload 声明的 org 与凭证映射的 org 一致：不匹配
// 即 403（防跨租户投递）。payloadOrg 为 0 视为未声明，放行（org 以凭证为准）。
func RequirePayloadOrg(c *gin.Context, payloadOrg int64) bool {
	tokenOrg := c.GetInt64("org_id")
	if payloadOrg != 0 && payloadOrg != tokenOrg {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": gin.H{"type": "org_mismatch", "message": "payload org does not match the ingest token"},
		})
		return false
	}
	return true
}

func bearerToken(header string) string {
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}
