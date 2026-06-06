package admin

import (
	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/debug"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/secret"
	"github.com/crosslink/internal/service"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// AdminHandlers holds all admin handler instances.
type AdminHandlers struct {
	Provider *ProviderHandler
	Model    *ModelHandler
	Key      *KeyHandler
	Usage    *UsageHandler
	System   *SystemHandler
	Debug    *DebugHandler
	License      *LicenseHandler
	Preferences  *PreferencesHandler
	Perms        gin.HandlerFunc
}

// AdminDeps groups the dependencies required to construct admin handlers.
type AdminDeps struct {
	DB             *gorm.DB
	RDB            *redis.Client
	Repos          *repository.Repos
	Svcs           *service.Services
	Resolver       *router.Resolver
	Registry       *provider.Registry
	Health         *provider.HealthTracker
	RetryBudget    *provider.RetryBudget
	CacheSvc       *service.CacheService
	SecretResolver *secret.SecretResolver
	EncStore       *secret.EncryptedDBStore
	DebugStore     *debug.Store
	Crypto         crypto.CryptoProvider
	Config         *config.Config
}

// ProvideAdminHandlers constructs all admin handlers from their dependencies.
// The nil auditSvc parameters are commercial extension hooks — the commercial
// overlay replaces them at build time.
func ProvideAdminHandlers(deps *AdminDeps) *AdminHandlers {
	return &AdminHandlers{
		Provider: NewProviderHandler(
			deps.Repos.ProviderRepo,
			deps.Repos.ProviderModelCRUDRepo,
			deps.Resolver,
			deps.Registry,
			deps.CacheSvc,
			deps.SecretResolver,
			deps.EncStore,
			nil, // auditSvc — commercial extension hook
			deps.Svcs.UsageSvc,
		deps.Svcs.LatencySvc,
		),
		Model: NewModelHandler(
			deps.Repos.ProviderModelCRUDRepo,
			deps.Resolver,
			deps.CacheSvc,
			nil, // auditSvc — commercial extension hook
		),
		Key: NewKeyHandler(
			deps.Svcs.KeySvc,
			deps.Repos.TeamRepo,
			deps.RDB,
			nil, // auditSvc — commercial extension hook
		),
		Usage: NewUsageHandler(deps.DB, ""),
		System: NewSystemHandler(
			deps.DB,
			deps.RDB,
			deps.Config.Admin,
			deps.Svcs.UsageSvc,
			deps.DebugStore,
			deps.Health,
			deps.RetryBudget,
			nil, // auditSvc — commercial extension hook
		),
		Debug: NewDebugHandler(
			deps.DebugStore,
			nil, // auditSvc — commercial extension hook
		),
		License: NewLicenseHandler(deps.DB, deps.Config),
		Preferences: NewPreferencesHandler(deps.Repos.UserRepo),
		Perms:   GetPermissionsHandler(deps.Repos.RoleRepo),
	}
}
