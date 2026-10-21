package service

import (
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/dialect"
	"github.com/crosslink/internal/repository"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// Services holds all service instances used by the application.
type Services struct {
	UsageSvc       *UsageService
	BudgetSvc      *BudgetService
	BudgetAlertSvc *BudgetAlertService
	BudgetCalSvc   *BudgetCalibrationService
	LatencySvc     *LatencyService
	CacheSvc       *CacheService
	KeySvc         *KeyService
	ActiveTracker  *ActiveRequestTracker
	IdemCache      *IdempotencyCache
	DataLensAggSvc *DataLensAggregatorService
}

// ProvideServices constructs all service instances from repos + infra dependencies.
// cacheRdb is the Redis client for the response cache (may be a dedicated
// instance so its eviction policy can differ from quota/budget keys); nil
// falls back to the shared rdb. markerTTL configures the F1′ idempotency
// markers ("" or invalid → 24h default).
func ProvideServices(repos *repository.Repos, rdb *redis.Client, cacheRdb *redis.Client, db *gorm.DB, cacheCfg *config.CacheConfig, cp crypto.CryptoProvider, dlCfg config.DataLensConfig, d dialect.Dialect, markerTTL, reapInterval string) *Services {
	if cacheRdb == nil {
		cacheRdb = rdb
	}
	ttl, err := time.ParseDuration(markerTTL)
	if err != nil || ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Services{
		UsageSvc:       NewUsageService(repos.UsageLogRepo),
		BudgetSvc:      NewBudgetServiceWithTTLs(rdb, ttl, parseReapInterval(reapInterval)),
		BudgetAlertSvc: NewBudgetAlertService(repos.BudgetAlertRepo),
		BudgetCalSvc:   NewBudgetCalibrationService(db, rdb),
		LatencySvc:     NewLatencyService(rdb),
		CacheSvc:       NewCacheService(cacheRdb, db, *cacheCfg),
		KeySvc:         NewKeyService(repos.APIKeyRepo, repos.APIKeyHashRepo, db, cp, rdb),
		ActiveTracker:  NewActiveRequestTracker(rdb),
		IdemCache:      NewIdempotencyCache(rdb),
		DataLensAggSvc: NewDataLensAggregatorService(db, d, dlCfg),
	}
}

// parseReapInterval parses gateway.budget_reap_interval; unparsable or
// negative values disable the independent loop (legacy hourly-only reap).
func parseReapInterval(s string) time.Duration {
	v, err := time.ParseDuration(s)
	if err != nil || v < 0 {
		return 0
	}
	return v
}
