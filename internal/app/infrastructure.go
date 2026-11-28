package app

import (
	"context"
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
	// Build strategy map (base + commercial extensions). Declared as vars so
	// the health-score closure can be injected after the HealthTracker exists.
	weightedStrategy := &router.WeightedRandomStrategy{}
	roundRobinStrategy := router.NewRoundRobinStrategy(deps.RDB)
	strategies := map[router.StrategyName]router.RoutingStrategy{
		router.StrategyWeightedRandom: weightedStrategy,
		router.StrategyRoundRobin:     roundRobinStrategy,
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
		deps.Extensions.AliasResolver,
	)
	// r78 availability bound (config gateway.resolve_lookup_timeout): a
	// bounded miss reaches the existing degraded negative-cache / typed-503
	// path instead of holding the single-flight probe for the dial timeout.
	if d, err := time.ParseDuration(deps.Config.Gateway.ResolveLookupTimeout); err == nil && d > 0 {
		resolver.SetResolveTimeout(d)
	}

	gatewaySvc := service.NewGatewayService(
		resolver, registry, deps.Svcs.LatencySvc, deps.Svcs.ActiveTracker, retryBudget,
	)

	// P4.5: wire the health-score closure into the resolver (cache-hit
	// demotion) and the two Community strategies (per-request Select, cache
	// hits included). The strategies get the resolver's 2s-cached view of the
	// score: their filterByHealth now runs on every request, and the raw
	// scorer would cost up to three Redis reads per primary per request. The
	// overlay's 4 strategies are wired separately in their main.go (B2). nil
	// rdb → skip (HealthScore would fail-open to 1.0 anyway, so wiring is
	// pointless).
	if deps.RDB != nil {
		resolver.SetHealthScoreFn(func(providerName, model string) float64 {
			return service.HealthScore(context.Background(), deps.RDB, health, providerName, model)
		})
		cachedHealthFn := resolver.CachedHealthScoreFn()
		weightedStrategy.SetHealthScoreFn(cachedHealthFn)
		roundRobinStrategy.SetHealthScoreFn(cachedHealthFn)
	}

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
