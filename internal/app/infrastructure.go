package app

import (
	"time"

	"github.com/crosslink/internal/admin"
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/secret"
	"github.com/crosslink/internal/service"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// Infrastructure holds the constructed infrastructure components:
// registry, health tracker, resolver, gateway service, retry budget, and registry sync.
type Infrastructure struct {
	Registry     *provider.Registry
	Health       *provider.HealthTracker
	Resolver     *router.Resolver
	GatewaySvc   *service.GatewayService
	RetryBudget  *provider.RetryBudget
	RegistrySync *provider.RegistrySync
	Classifier   *service.ErrorClassifier
}

// InfraDeps holds the dependencies needed to construct Infrastructure.
type InfraDeps struct {
	Config         *config.Config
	DB             *gorm.DB
	RDB            *redis.Client
	Repos          *repository.Repos
	Svcs           *service.Services
	SecretResolver *secret.SecretResolver
	Extensions     *Extensions
}

// ProvideInfrastructure constructs the infrastructure layer: provider registry,
// health tracker, retry budget, router resolver, and gateway service.
func ProvideInfrastructure(deps *InfraDeps) *Infrastructure {
	// Build strategy map (base + commercial extensions)
	strategies := map[router.StrategyName]router.RoutingStrategy{
		router.StrategyWeightedRandom: &router.WeightedRandomStrategy{},
		router.StrategyRoundRobin:     router.NewRoundRobinStrategy(deps.RDB),
	}
	for name, s := range deps.Extensions.ExtraStrategies {
		strategies[name] = s
	}

	rc := admin.LoadResilienceConfig(deps.DB)

	registry := provider.NewRegistry()
	RegisterProvidersFromDB(deps.Config, deps.DB, registry, deps.SecretResolver)

	health := provider.NewHealthTrackerWithConfig(
		rc.CircuitBreakerThreshold,
		time.Duration(rc.CircuitBreakerDuration)*time.Second,
	)
	health.SetPersistentCooldown(time.Duration(rc.PersistentCooldown) * time.Second)
	health.SetRetryAfterBounds(time.Duration(rc.RetryAfterMin)*time.Second, time.Duration(rc.RetryAfterMax)*time.Second)

	retryBudget := provider.NewRetryBudget(deps.RDB, rc.RetryBudgetPerSecond)

	// Error classifier: TTL-cached rule table distinguishing persistent (quota/billing)
	// failures from transient ones. Loads once synchronously here; app.go runs the
	// background refresh loop bound to appCtx.
	classifier := service.NewErrorClassifier(service.AdaptErrorRuleLoader(deps.Repos.ErrorRuleRepo), 30*time.Second)

	resolver := router.NewResolver(
		registry, deps.Repos.ProviderModelRepo, health,
		strategies, deps.Svcs.LatencySvc, deps.Svcs.ActiveTracker, deps.SecretResolver,
	)

	gatewaySvc := service.NewGatewayService(
		resolver, registry, deps.Svcs.LatencySvc, deps.Svcs.ActiveTracker, retryBudget,
	)

	var registrySync *provider.RegistrySync
	if deps.RDB != nil {
		registrySync = provider.NewRegistrySync(deps.RDB, registry, deps.DB, deps.SecretResolver)
	}

	return &Infrastructure{
		Registry:     registry,
		Health:       health,
		Resolver:     resolver,
		GatewaySvc:   gatewaySvc,
		RetryBudget:  retryBudget,
		RegistrySync: registrySync,
		Classifier:   classifier,
	}
}
