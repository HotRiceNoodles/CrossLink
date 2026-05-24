package service

import (
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/repository"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// Services holds all service instances used by the application.
type Services struct {
	UsageSvc      *UsageService
	BudgetSvc     *BudgetService
	BudgetAlertSvc *BudgetAlertService
	BudgetCalSvc  *BudgetCalibrationService
	LatencySvc    *LatencyService
	CacheSvc      *CacheService
	KeySvc        *KeyService
	ActiveTracker *ActiveRequestTracker
	IdemCache     *IdempotencyCache
}

// ProvideServices constructs all service instances from repos + infra dependencies.
func ProvideServices(repos *repository.Repos, rdb *redis.Client, db *gorm.DB, cacheCfg *config.CacheConfig, cp crypto.CryptoProvider) *Services {
	return &Services{
		UsageSvc:      NewUsageService(repos.UsageLogRepo),
		BudgetSvc:     NewBudgetService(rdb),
		BudgetAlertSvc: NewBudgetAlertService(repos.BudgetAlertRepo),
		BudgetCalSvc:  NewBudgetCalibrationService(db, rdb),
		LatencySvc:    NewLatencyService(rdb),
		CacheSvc:      NewCacheService(rdb, db, *cacheCfg),
		KeySvc:        NewKeyService(repos.APIKeyRepo, repos.APIKeyHashRepo, db, cp, rdb),
		ActiveTracker: NewActiveRequestTracker(rdb),
		IdemCache:     NewIdempotencyCache(rdb),
	}
}
