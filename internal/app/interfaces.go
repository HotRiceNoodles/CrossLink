package app

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/debug"
	"github.com/crosslink/internal/dialect"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/secret"
	"github.com/crosslink/internal/service"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// GateInterface abstracts license tier checking.
type GateInterface interface {
	RequirePro() error
	RequireEnterprise() error
	CurrentTier() string
}

// NoopGate is the Community default.
type NoopGate struct{}

func (NoopGate) RequirePro() error       { return nil }
func (NoopGate) RequireEnterprise() error { return nil }
func (NoopGate) CurrentTier() string      { return "community" }

// AppDeps holds shared services for Commercial Extensions to access.
// Set by Community during FullSetup, consumed by Commercial ExtraRoutes.
type AppDeps struct {
	DB             *gorm.DB
	Dialect        dialect.Dialect
	RDB            *redis.Client
	SecretResolver *secret.SecretResolver
	EncStore       *secret.EncryptedDBStore
	ActiveKeyPtr   *string
	Resolver       *router.Resolver
	CacheSvc       *service.CacheService
	Health         *provider.HealthTracker
	RetryBudget    *provider.RetryBudget
	LatencySvc     *service.LatencyService
	UsageSvc       *service.UsageService
	GuardrailSvc   *guardrail.GuardrailService
	Config         *config.Config
	PermCache      *middleware.PermissionCache
	KeySvc         *service.KeyService
	DebugStore     *debug.Store
	Crypto         crypto.CryptoProvider
	OrgCache       *middleware.OrgCache
	AuditSvc       *service.AuditService // set by commercial ExtraPublicRoutes; nil in Community
	VideoTaskSvc   *service.VideoTaskService
	DataLensStore  repository.MetricsStore
	DataLensRepo   *repository.DataLensRepository
	AppCtx         context.Context // application-lifetime context for background goroutines
}

// Extensions holds Commercial-only plugins injected at startup.
type Extensions struct {
	ExtraProviders   []func(*config.Config, *gorm.DB)
	ExtraStrategies  map[router.StrategyName]router.RoutingStrategy
	ExtraMiddlewares []func(*gin.RouterGroup, *Extensions)
	ExtraRoutes      func(*gin.RouterGroup, *Extensions)
	ExtraMCPRoutes   func(*gin.RouterGroup, *Extensions) // mcpGroup (independent route group)
	ExtraPublicRoutes  func(*gin.Engine, *Extensions)  // public routes (no auth, e.g. SSO login)
	ExtraEngineRoutes  func(*gin.Engine, *Extensions)  // main router (for docs, etc.)
	MCPEncSetter       func(encStore *secret.EncryptedDBStore) // called by app.go after encStore is ready
	Gate             GateInterface
	Deps             *AppDeps
}

func NoopExtensions() *Extensions {
	return &Extensions{
		Gate:            NoopGate{},
		ExtraStrategies: map[router.StrategyName]router.RoutingStrategy{},
	}
}
