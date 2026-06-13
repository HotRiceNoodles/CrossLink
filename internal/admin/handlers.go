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
	ErrorRule    *ErrorRuleHandler
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
	AuditSvc       *service.AuditService // set by commercial build; nil in Community
}

// ProvideAdminHandlers constructs all admin handlers from their dependencies.
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
			deps.AuditSvc,
			deps.Svcs.UsageSvc,
		deps.Svcs.LatencySvc,
		),
		Model: NewModelHandler(
			deps.Repos.ProviderModelCRUDRepo,
			deps.Resolver,
			deps.CacheSvc,
			deps.AuditSvc,
		),
		Key: NewKeyHandler(
			deps.Svcs.KeySvc,
			deps.Repos.TeamRepo,
			deps.RDB,
			deps.AuditSvc,
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
			deps.AuditSvc,
		),
		Debug: NewDebugHandler(
			deps.DebugStore,
			deps.AuditSvc,
		),
		License: NewLicenseHandler(deps.DB, deps.Config),
		Preferences: NewPreferencesHandler(deps.Repos.UserRepo, deps.AuditSvc),
		ErrorRule: NewErrorRuleHandler(deps.Repos.ErrorRuleRepo, deps.AuditSvc),
		Perms:   GetPermissionsHandler(deps.Repos.RoleRepo),
	}
}
