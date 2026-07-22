package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/crosslink/internal/admin"
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/debug"
	"github.com/crosslink/internal/dialect"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/handler"
	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/secret"
	"github.com/crosslink/internal/service"
	"github.com/crosslink/internal/version"
	"github.com/crosslink/internal/worker"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

func FullSetup(cfg *config.Config, db *gorm.DB, rdb *redis.Client, ext *Extensions, dia dialect.Dialect) {
	if ext == nil {
		ext = NoopExtensions()
	}

	buildAuth(db, cfg)

	// H1: warn loudly when the static config auth key is set. This key bypasses
	// all per-API-key enforcement (budget, model/route allow-lists, rate limits)
	// and has no expiry or disable mechanism — if leaked it is full, untraceable
	// access. Prefer database-managed API keys; if used, treat as a root credential.
	if cfg.Gateway.AuthKey != "" {
		slog.Warn("gateway.auth_key is set — this static key bypasses all per-key limits and cannot be expired/disabled; use only as a root credential and rotate via config restart if compromised")
	}

	// Configure the SSRF internal allowlist for outbound provider/MCP/video
	// connections. Empty = strict (block all restricted ranges). On invalid CIDRs
	// we keep the strict default and log loudly rather than fail to boot.
	if len(cfg.Gateway.InternalAllowCIDRs) > 0 {
		if err := guardrail.SetInternalAllowlist(cfg.Gateway.InternalAllowCIDRs); err != nil {
			slog.Error("invalid gateway.internal_allow_cidrs; SSRF allowlist stays strict", "error", err)
		} else {
			slog.Info("SSRF internal allowlist active", "cidrs", cfg.Gateway.InternalAllowCIDRs)
		}
	}

	// Crypto provider (standard or GM mode)
	cryptoProvider, err := crypto.NewProvider(cfg.Crypto.Mode)
	if err != nil {
		slog.Error("failed to create crypto provider", "error", err)
		os.Exit(1)
	}
	slog.Info("crypto provider initialized", "mode", cfg.Crypto.Mode)

	if err := config.SeedProviders(db, "configs/providers.yaml"); err != nil {
		slog.Warn("failed to seed providers", "error", err)
	}
	if err := config.SeedPromptTemplates(db, "configs/prompt_templates.yaml"); err != nil {
		slog.Warn("failed to seed prompt templates", "error", err)
	}

	// Optional zero-cost demo mode: seed a mock provider + embedded demo key.
	// Disabled by default; only enabled in the demo deployment. Errors are
	// non-fatal (logged) — a refused seed never blocks startup.
	if cfg.Demo.Enabled {
		if err := service.EnsureDemoSeed(db, cfg.Database.Driver, cfg.Demo.APIKey, cryptoProvider); err != nil {
			slog.Warn("demo seed skipped", "error", err)
		} else {
			slog.Info("demo mode seeded", "provider", "mock-demo")
		}
	}

	// Secrets
	secrets := buildSecrets(db, cfg, ext, cryptoProvider, rdb)
	defer secrets.CleanupCancel()

	// Repositories
	repos := repository.ProvideRepos(db)

	// Permission cache for RBAC
	permCache := middleware.NewPermissionCache(repos.RoleRepo)
	if err := permCache.Load(); err != nil {
		slog.Warn("failed to load permission cache", "error", err)
	}

	// Services
	svcs := service.ProvideServices(repos, rdb, db, &cfg.Cache, cryptoProvider, cfg.DataLens, dia)
	guardrailSvc := guardrail.NewGuardrailService(db, rdb)

	// Load middleware log config from DB
	var mlCfg model.SystemSetting
	if db.Where("key = ?", "log_middleware_errors").First(&mlCfg).Error == nil {
		var cfg service.MiddlewareLogConfig
		if json.Unmarshal([]byte(mlCfg.Value), &cfg) == nil {
			svcs.UsageSvc.SetMiddlewareLogConfig(&cfg)
		}
	}
	guardrailSvc.RefreshSettings(context.Background())

	// Background task context (for graceful shutdown)
	appCtx, appCancel := context.WithCancel(context.Background())
	teamCache := middleware.NewTeamCache(repos.TeamRepo, appCtx)
	orgCache := middleware.NewOrgCache(repos.OrgRepo, appCtx)
	// Background refresh: keep permission cache in sync across instances
	permCache.RunRefreshLoop(appCtx, 60*time.Second)

	// Load log_content setting into cache
	var logContentSetting model.SystemSetting
	if err := db.Where("key = ?", "log_content").First(&logContentSetting).Error; err == nil {
		svcs.UsageSvc.SetContentLogEnabled(logContentSetting.Value == "true")
	}

	// Debug store
	debugStore := debug.NewStore(100, 10*1024*1024)
	var debugModeSetting model.SystemSetting
	if err := db.Where("key = ?", "debug_mode").First(&debugModeSetting).Error; err == nil {
		debugStore.SetEnabled(debugModeSetting.Value == "true")
	}

	// Wire guardrail engine callbacks
	guardrailSvc.SetIsDebugEnabled(debugStore.IsEnabled)
	guardrailSvc.SetRedisClient(rdb)
	if ext.Gate != nil {
		guardrailSvc.SetCheckLicenseTier(ext.Gate.CurrentTier)
	}

	// Cache service configuration
	var cacheEnabled model.SystemSetting
	if err := db.Where("key = ?", "cache_enabled").First(&cacheEnabled).Error; err == nil {
		svcs.CacheSvc.SetEnabled(cacheEnabled.Value == "true")
	}
	var cacheDefaultTTL model.SystemSetting
	if err := db.Where("key = ?", "cache_default_ttl").First(&cacheDefaultTTL).Error; err == nil {
		if d, err := time.ParseDuration(cacheDefaultTTL.Value); err == nil {
			svcs.CacheSvc.SetDefaultTTL(d)
		}
	}
	var cacheEmbeddingsTTL model.SystemSetting
	if err := db.Where("key = ?", "cache_embeddings_ttl").First(&cacheEmbeddingsTTL).Error; err == nil {
		if d, err := time.ParseDuration(cacheEmbeddingsTTL.Value); err == nil {
			svcs.CacheSvc.SetEmbeddingsTTL(d)
		}
	}

	// Infrastructure layer
	infra := ProvideInfrastructure(&InfraDeps{
		Config:         cfg,
		DB:             db,
		RDB:            rdb,
		Repos:          repos,
		Svcs:           svcs,
		SecretResolver: secrets.SecretResolver,
		Extensions:     ext,
	})

	// Background refresh: keep resilience cooldowns/thresholds in sync without restart
	go admin.RunResilienceRefreshLoop(appCtx, db, infra.Health, 30*time.Second)
	// Background refresh: keep the error-classification rule table hot-reloaded
	go infra.Classifier.RunRefreshLoop(appCtx)

	// Handlers

	// Populate extension deps for Commercial
	ext.Deps = &AppDeps{
		DB:             db,
		Dialect:        dia,
		RDB:            rdb,
		SecretResolver: secrets.SecretResolver,
		EncStore:       secrets.EncStore,
		ActiveKeyPtr:   secrets.ActiveKeyPtr,
		CacheSvc:       svcs.CacheSvc,
		UsageSvc:       svcs.UsageSvc,
		Resolver:       infra.Resolver,
		Health:         infra.Health,
		RetryBudget:    infra.RetryBudget,
		LatencySvc:     svcs.LatencySvc,
		GuardrailSvc:   guardrailSvc,
		Config:         cfg,
		PermCache:      permCache,
		KeySvc:         svcs.KeySvc,
		DebugStore:     debugStore,
		Crypto:         cryptoProvider,
		OrgCache:       orgCache,
		DataLensStore:  repository.NewPgMetricsStore(db, dia),
		DataLensRepo:   repos.DataLensRepo,
		AppCtx:         appCtx,
	}
	anthropicHandler := handler.NewAnthropicHandler(infra.GatewaySvc, infra.Resolver, svcs.UsageSvc, svcs.IdemCache, nil)
	openaiHandler := handler.NewOpenAIHandler(infra.Resolver, svcs.UsageSvc, svcs.LatencySvc, nil, svcs.ActiveTracker, svcs.IdemCache, infra.RetryBudget, nil)
	usageQueryHandler := handler.NewUsageQueryHandler(svcs.BudgetSvc)
	portalUsageHandler := handler.NewPortalUsageHandler(svcs.BudgetSvc)
	templateCatalogHandler := handler.NewTemplateCatalogHandler(db)

	// Video gateway
	videoTaskSvc := service.NewVideoTaskService(rdb, secrets.EncStore, infra.Registry, svcs.UsageSvc)
	ext.Deps.VideoTaskSvc = videoTaskSvc // expose to commercial overlay
	videoHandler := handler.NewVideoHandler(
		videoTaskSvc, infra.Resolver, infra.Health,
		svcs.UsageSvc, svcs.ActiveTracker, svcs.IdemCache, infra.RetryBudget,
	)

	// Inject the error classifier into every fallback-engine consumer (NB1 chain).
	infra.GatewaySvc.SetClassifier(infra.Classifier)
	infra.GatewaySvc.SetGuardRDB(rdb)
	openaiHandler.SetClassifier(infra.Classifier)
	openaiHandler.SetGuardRDB(rdb)
	openaiHandler.SetBudgetSvc(svcs.BudgetSvc)
	anthropicHandler.SetBudgetSvc(svcs.BudgetSvc)
	videoHandler.SetClassifier(infra.Classifier)
	videoHandler.SetGuardRDB(rdb)

	usageWorkers := worker.NewPool(15, 1000)
	handler.SetUsageWorkers(usageWorkers)
	modelsHandler := handler.NewModelsHandler(db)

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	if len(cfg.Server.TrustedProxies) > 0 {
		r.SetTrustedProxies(cfg.Server.TrustedProxies)
		slog.Warn("TrustedProxies configured — ensure only known reverse proxy IPs are listed to prevent X-Forwarded-For spoofing", "proxies", cfg.Server.TrustedProxies)
	} else {
		r.SetTrustedProxies(nil)
	}

	// Global middleware
	r.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 50<<20)
		c.Next()
	})
	r.Use(middleware.RequestID())
	r.Use(middleware.Tracing())
	r.Use(middleware.Recovery())
	r.Use(middleware.CORS(middleware.CORSConfig{AllowedOrigins: cfg.CORS.AllowedOrigins}))
	r.Use(middleware.Logger())
	r.Use(middleware.Metrics())

	// Health endpoint (no auth)
	r.GET("/health", healthCheck(db))

	// Prometheus metrics endpoint (requires gateway auth key)
	r.GET("/metrics", middleware.GatewayMetricsAuth(cfg.Gateway.AuthKey), gin.WrapH(promhttp.Handler()))

	// NOTE: the gateway serves API traffic only. Frontend static files (admin SPA
	// + customer portal) are served by the reverse proxy (Caddy/nginx) or a
	// separate static server — the backend intentionally has no knowledge of any
	// frontend entry filename. See deployments/Caddyfile and README.

	// Commercial public route extension point (SSO login, callback, metadata)
	// Must run BEFORE login route registration so that commercial builds can
	// inject AuditSvc into deps for LoginHandler/ChangeForcedPasswordHandler.
	if ext.ExtraPublicRoutes != nil {
		ext.ExtraPublicRoutes(r, ext)
	}

	// Admin handlers — created after ExtraPublicRoutes so commercial builds can inject AuditSvc.
	templateRegistry := service.NewTemplateRegistry(db)
	var templateSync *service.TemplateRegistrySync
	if rdb != nil {
		templateSync = service.NewTemplateRegistrySync(rdb, templateRegistry, db)
	}
	handlers := admin.ProvideAdminHandlers(&admin.AdminDeps{
		DB:             db,
		RDB:            rdb,
		Repos:          repos,
		Svcs:           svcs,
		Resolver:       infra.Resolver,
		Registry:       infra.Registry,
		Health:         infra.Health,
		RetryBudget:    infra.RetryBudget,
		CacheSvc:       svcs.CacheSvc,
		SecretResolver: secrets.SecretResolver,
		EncStore:       secrets.EncStore,
		DebugStore:     debugStore,
		Crypto:         cryptoProvider,
		Config:         cfg,
		AuditSvc:       ext.Deps.AuditSvc,
		TemplateRegistry: templateRegistry,
		TemplateSync:     templateSync,
	})

	// Wire registry sync: notify other instances on provider mutations,
	// subscribe for reload notifications from other instances.
	if infra.RegistrySync != nil {
		registrySyncFn := func(action, providerName string) {
			if action == "remove" {
				infra.RegistrySync.NotifyRemove(providerName)
			} else {
				infra.RegistrySync.NotifyReload(providerName)
			}
		}
		handlers.Provider.OnRegistryChange = registrySyncFn
		handlers.Onboarding.SetOnRegistryChange(registrySyncFn)
		go infra.RegistrySync.Start(appCtx)
	}
	// VCR recording/playback: construct fixture repo, enable on providers + mock.
	fixtureRepo := repository.NewFixtureRepo(db)
	handlers.Provider.SetFixtureStore(fixtureRepo)
	provider.SetGlobalFixtureStore(fixtureRepo)
	// TemplateRegistrySync: keep prompt-template cache consistent across instances.
	if templateSync != nil {
		go templateSync.Start(appCtx)
	}
	// Login endpoint (no auth, rate limited)
	// Registered after ExtraPublicRoutes so deps.AuditSvc is available.
	captchaGate := buildCaptchaGate(cfg.Captcha, rdb, []byte(cfg.Admin.JWTSecret))
	// Captcha issue: pre-auth, image generation is cheap; dedicated per-IP
	// issue rate limit deferred (login limiter already caps the real attack
	// surface — failed logins). See docs/plans/2026-07-03-login-captcha-design.md §3.4.
	r.GET("/admin/api/auth/captcha/issue", admin.CaptchaIssueHandler(captchaGate))
	r.POST("/admin/api/auth/login", middleware.LoginRateLimit(rdb, 10, 15*time.Minute), admin.LoginHandler(repos.UserRepo, repos.TeamRepo, repos.RoleRepo, repos.OrgRepo, cfg.Admin, ext.Deps.AuditSvc, cryptoProvider, captchaGate))
	r.POST("/admin/api/auth/logout", admin.LogoutHandler())

	// Lightweight auth endpoints (JWT required, exempt from rate limit)
	authGroup := r.Group("/admin/api")
	authGroup.Use(admin.JWTAuthMiddleware(cfg.Admin, db, cryptoProvider))
	authGroup.Use(middleware.OrgResolve())
	{
		authGroup.GET("/version", func(c *gin.Context) {
			c.JSON(200, gin.H{"version": version.Version})
		})
		authGroup.GET("/auth/permissions", handlers.Perms)
		authGroup.POST("/auth/change-forced-password", admin.ChangeForcedPasswordHandler(repos.UserRepo, repos.RoleRepo, repos.OrgRepo, repos.TeamRepo, cfg.Admin, ext.Deps.AuditSvc, cryptoProvider))
	}

	// Admin API (JWT auth + rate limit)
	adminGroup := r.Group("/admin/api")
	adminGroup.Use(admin.JWTAuthMiddleware(cfg.Admin, db, cryptoProvider))
	adminGroup.Use(middleware.OrgResolve())
	adminGroup.Use(middleware.AdminRateLimit(rdb, 300, time.Minute, ""))
	adminGroup.Use(middleware.CSRFGuard())
	adminGroup.Use(middleware.GuardrailsRequest(guardrailSvc))
	{
		// Providers
		// Intentionally no RequireAction: read-only metadata endpoint, not sensitive.
		adminGroup.GET("/adapters", handlers.Provider.ListAdapters)
		adminGroup.GET("/providers", middleware.RequireAction(permCache, "provider:list"), handlers.Provider.List)
		adminGroup.POST("/providers", middleware.RequireAction(permCache, "provider:create"), handlers.Provider.Create)
		adminGroup.PUT("/providers/:id", middleware.RequireAction(permCache, "provider:update"), handlers.Provider.Update)
		adminGroup.DELETE("/providers/:id", middleware.RequireAction(permCache, "provider:delete"), handlers.Provider.Delete)
		adminGroup.POST("/providers/:id/test", middleware.RequireAction(permCache, "provider:test"), handlers.Provider.Test)
		adminGroup.GET("/providers/:id/models", middleware.RequireAction(permCache, "provider:list"), handlers.Provider.ListModels)
		// Onboarding wizard: stateless upstream probe (no RequireAction — login-only,
		// like /adapters and /system:info) and atomic provider+models+key creator.
		adminGroup.POST("/providers/probe", handlers.Onboarding.Probe)
		adminGroup.POST("/system/onboarding", handlers.Onboarding.Commit)

		// Models
		adminGroup.GET("/models", middleware.RequireAction(permCache, "model:list"), handlers.Model.List)
		adminGroup.POST("/models", middleware.RequireAction(permCache, "model:create"), handlers.Model.Create)
		adminGroup.PUT("/models/:id", middleware.RequireAction(permCache, "model:update"), handlers.Model.Update)
		adminGroup.DELETE("/models/:id", middleware.RequireAction(permCache, "model:delete"), handlers.Model.Delete)

		adminGroup.GET("/error-rules", middleware.RequireAction(permCache, "error_rule:list"), handlers.ErrorRule.List)
		adminGroup.POST("/error-rules", middleware.RequireAction(permCache, "error_rule:create"), handlers.ErrorRule.Create)
		adminGroup.PUT("/error-rules/:id", middleware.RequireAction(permCache, "error_rule:update"), handlers.ErrorRule.Update)
		adminGroup.DELETE("/error-rules/:id", middleware.RequireAction(permCache, "error_rule:delete"), handlers.ErrorRule.Delete)

		// Keys
		adminGroup.POST("/keys", middleware.RequireAction(permCache, "key:create"), handlers.Key.Create)
		adminGroup.GET("/keys", middleware.RequireAction(permCache, "key:list"), handlers.Key.List)
		adminGroup.PUT("/keys/:id", middleware.RequireAction(permCache, "key:update"), handlers.Key.Update)
		adminGroup.DELETE("/keys/:id", middleware.RequireAction(permCache, "key:delete"), handlers.Key.Delete)
		adminGroup.POST("/keys/:id/regenerate", middleware.RequireAction(permCache, "key:regenerate"), handlers.Key.Regenerate)

		// Usage
		adminGroup.GET("/usage", middleware.RequireAction(permCache, "usage:list"), handlers.Usage.List)

		adminGroup.GET("/usage/stats", middleware.RequireAction(permCache, "usage:stats"), handlers.Usage.Stats)
		adminGroup.GET("/usage/daily", middleware.RequireAction(permCache, "usage:list"), handlers.Usage.DailyTrend)
		adminGroup.GET("/usage/models", middleware.RequireAction(permCache, "usage:list"), handlers.Usage.ModelDistribution)
		adminGroup.GET("/usage/templates", middleware.RequireAction(permCache, "usage:list"), handlers.Usage.TemplateStats)
		adminGroup.GET("/usage/reconciliation/export", middleware.RequireAction(permCache, "usage:stats"), handlers.Usage.ReconciliationExport)
		adminGroup.GET("/usage/team-stats", middleware.RequireAction(permCache, "usage:stats"), handlers.Usage.TeamStats)
		adminGroup.GET("/routing/stats", middleware.RequireAction(permCache, "routing:stats"), handlers.Routing.Stats)

		// Prompt templates (context engineering). Super-admin only (AdminExclusiveActions).
		adminGroup.GET("/templates", middleware.RequireAction(permCache, "template:list"), handlers.Templates.List)
		adminGroup.POST("/templates", middleware.RequireAction(permCache, "template:create"), handlers.Templates.Create)
		adminGroup.GET("/templates/:id", middleware.RequireAction(permCache, "template:list"), handlers.Templates.Get)
		adminGroup.PUT("/templates/:id", middleware.RequireAction(permCache, "template:update"), handlers.Templates.Update)
		adminGroup.DELETE("/templates/:id", middleware.RequireAction(permCache, "template:delete"), handlers.Templates.Delete)
		adminGroup.POST("/templates/:id/preview", middleware.RequireAction(permCache, "template:list"), handlers.Templates.Preview)

		// System info (self-service, no RequireAction)
		adminGroup.GET("/system/info", handlers.System.Info)
		adminGroup.POST("/system/password", middleware.RequireAction(permCache, "system:password"), handlers.System.ChangePassword)

		// License
		adminGroup.GET("/license/status", middleware.RequireAction(permCache, "license:view"), handlers.License.Status)
		// User preferences (self-service, no RequireAction)
		adminGroup.GET("/user/preferences", handlers.Preferences.Get)
		adminGroup.PUT("/user/preferences", handlers.Preferences.Update)

		// Debug
		adminGroup.GET("/debug/entries", middleware.RequireAction(permCache, "debug:list"), handlers.Debug.List)
		adminGroup.GET("/debug/entries/:seq", middleware.RequireAction(permCache, "debug:list"), handlers.Debug.Get)
		adminGroup.POST("/debug/entries/:seq/replay", middleware.RequireAction(permCache, "debug:list"), handlers.Debug.Replay)
		adminGroup.DELETE("/debug/entries", middleware.RequireAction(permCache, "debug:clear"), handlers.Debug.Clear)

		// Commercial route extension point
		if ext.ExtraRoutes != nil {
			slog.Info("registering extra admin routes via extension point")
			ext.ExtraRoutes(adminGroup, ext)
		}
	}

	// Draining manager for graceful shutdown of in-flight gateway streams
	dm := NewDrainingManager()

	// Gateway endpoints (auth + model permission + rate limit)
	gwGroup := r.Group("/")
	gwGroup.Use(dm.Middleware())
	concurrencyLimit := cfg.Gateway.ConcurrencyLimit
	if concurrencyLimit <= 0 {
		concurrencyLimit = 2000
	}
	gwGroup.Use(middleware.ConcurrencyLimit(concurrencyLimit))
	gwGroup.Use(middleware.ReadBody(10 << 20))
	gwGroup.Use(middleware.ContextAssembler(templateRegistry, ext.AssemblerHook))
	gwGroup.Use(debug.Middleware(debugStore))
	gwGroup.Use(middleware.UsageLog(svcs.UsageSvc))
	gwGroup.Use(middleware.AuthFailureLimit(rdb, 20, 15*time.Minute, ""))
	gwGroup.Use(middleware.Auth(cfg.Gateway.AuthKey, svcs.KeySvc, rdb, ext.IPPolicy))
	gwGroup.Use(middleware.RequireModel())
	gwGroup.Use(middleware.OrgResolve())
	gwGroup.Use(middleware.GuardrailsRequest(guardrailSvc))
	gwGroup.Use(middleware.Cache(svcs.CacheSvc, cryptoProvider))
	gwGroup.Use(middleware.RateLimit(rdb, cfg.RateLimit.RPM, teamCache, orgCache))
	gwGroup.Use(middleware.TPMLimit(rdb, cfg.RateLimit.TPM, teamCache, orgCache, cfg.RateLimit.Reservation, cfg.RateLimit.FailClosed))
	gwGroup.Use(middleware.BudgetCheck(svcs.BudgetSvc, teamCache, orgCache))
	gwGroup.Use(middleware.ReportTokens(rdb, orgCache))
	gwGroup.Use(middleware.ReportBudgetUsage(svcs.BudgetSvc, svcs.BudgetAlertSvc, teamCache, orgCache))
	gwGroup.Use(middleware.RoutingStats(rdb))
	// Commercial middleware extension point
	for _, mw := range ext.ExtraMiddlewares {
		mw(gwGroup, ext)
	}

	{
		gwGroup.POST("/v1/messages", middleware.RequireRoute("anthropic"), anthropicHandler.HandleMessages)
		gwGroup.POST("/v1/chat/completions", middleware.RequireRoute("openai"), openaiHandler.HandleChatCompletions)
		gwGroup.GET("/v1/models", modelsHandler.ListModels)
		gwGroup.POST("/v1/videos", videoHandler.CreateVideo)
		gwGroup.GET("/v1/videos/:id", videoHandler.GetVideo)
		gwGroup.GET("/v1/videos/:id/content", videoHandler.GetVideoContent)

		// Commercial gateway route extension point — inject multimodal/new-protocol
		// public endpoints (e.g. /v1/images/generations, /v1/audio/*, /v1/embeddings, /v1/batch).
		// Routes registered here inherit the full gwGroup middleware chain.
		if ext.ExtraGatewayRoutes != nil {
			ext.ExtraGatewayRoutes(gwGroup, ext)
		}
	}

	// Self-service usage query: key holders read their own real-time quota.
	// Lightweight group — only gateway Auth (no cache/guardrail/budget middleware).
	usageGroup := r.Group("/")
	usageGroup.Use(middleware.Auth(cfg.Gateway.AuthKey, svcs.KeySvc, rdb, ext.IPPolicy))
	usageGroup.GET("/v1/usage", usageQueryHandler.GetUsage)
	// Portal self-service API: same auth, but returns the portal-owned clean
	// contract (decoupled from /v1/usage internal shape). See PortalUsageHandler.
	usageGroup.GET("/portal/api/usage", portalUsageHandler.GetUsage)
	// Consumer discovery: key holders list available prompt templates (metadata only,
	// prompt content stays server-side) + ready-to-use curl examples. See §B.
	usageGroup.GET("/v1/templates", templateCatalogHandler.List)

	// MCP gateway route extension point (independent route group from gwGroup)
	if cfg.MCP.Enabled && ext.ExtraMCPRoutes != nil {
		mcpGroup := r.Group("/mcp")
		ext.ExtraMCPRoutes(mcpGroup, ext)
	}

	// Commercial engine-level routes (docs, etc.)
	if ext.ExtraEngineRoutes != nil {
		ext.ExtraEngineRoutes(r, ext)
	}

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: 0, // disabled — SSE streaming relies on per-request context deadlines
	}

	// Cleanup expired key hashes every 10 minutes
	// Budget calibration every hour
	go svcs.BudgetCalSvc.Run(appCtx)
	// DataLens aggregation background goroutine
	if cfg.DataLens.Enabled {
		if dia.PartitionSupport() == dialect.PartitionNative {
			var relkind string
			db.Raw("SELECT relkind FROM pg_class WHERE relname = 'usage_logs'").Scan(&relkind)
			if relkind != "p" {
				slog.Warn("usage_logs is not partitioned. Run 'crosslink migrate-partition' to enable partitioning. DataLens aggregation will use unpartitioned table (slower).")
			}
		}
		go svcs.DataLensAggSvc.Run(appCtx)
	}
	// Update cache size gauge periodically via approximate counter
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				stats := svcs.CacheSvc.QuickStats(appCtx)
				middleware.RecordCacheSize(float64(stats.TotalEntries))
				for _, ms := range stats.ByModel {
					middleware.RecordCacheSizeByModel(ms.Model, float64(ms.Count))
				}
			case <-appCtx.Done():
				return
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				deleted, err := repos.APIKeyHashRepo.DeleteExpired(appCtx)
				if err != nil {
					slog.Warn("failed to cleanup expired hashes", "error", err)
				} else if deleted > 0 {
					slog.Info("cleaned up expired key hashes", "count", deleted)
				}
			case <-appCtx.Done():
				return
			}
		}
	}()

	go func() {
		slog.Info("starting gateway", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("failed to start server", "error", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("shutting down server...", "active_requests", dm.ActiveCount())

	// Phase 1: Drain in-flight gateway streams
	drainTimeout := 60 * time.Second
	remaining := dm.Drain(drainTimeout)
	if remaining > 0 {
		slog.Warn("drain timeout exceeded, forcing shutdown", "remaining", remaining)
	} else {
		slog.Info("all in-flight requests completed during drain")
	}

	// Phase 2: Stop accepting new connections, wait for remaining
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server forced to shutdown", "error", err)
	}

	// Phase 3: Flush usage worker pool
	workerCtx, workerCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer workerCancel()
	usageWorkers.Shutdown(workerCtx)

	// Phase 4: Cancel background goroutines
	videoTaskSvc.Close()
	appCancel()

	// Phase 5: Database cleanup
	if err := dia.Shutdown(db); err != nil {
		slog.Error("database shutdown error", "error", err)
	}

	slog.Info("server exited")
}

func healthCheck(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		sqlDB, err := db.DB()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error"})
			return
		}
		if err := sqlDB.Ping(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

func RegisterProvidersFromDB(cfg *config.Config, db *gorm.DB, registry *provider.Registry, secretResolver *secret.SecretResolver) {
	var providers []model.Provider

	if err := db.Where("status = 1").Find(&providers).Error; err != nil {
		slog.Error("failed to load providers", "error", err)
		return
	}

	for _, p := range providers {
		// Resolve secrets for the in-memory provider instance.
		// Runtime requests resolve again via router.Resolver — this startup
		// resolution is needed for syncRegistry (admin CRUD) and provider
		// types that store apiKey as a fallback (e.g. Anthropic).
		if secretResolver != nil {
			resolvedKey, err := secretResolver.Resolve(context.Background(), p.APIKey)
			if err != nil {
				slog.Warn("resolve provider key failed", "name", p.Name, "error", err)
				continue
			}
			p.APIKey = resolvedKey

			// Resolve extra_config sensitive fields
			if len(p.ExtraConfig) > 0 {
				var extraMap map[string]any
				if json.Unmarshal(p.ExtraConfig, &extraMap) == nil {
					if err := secretResolver.ResolveExtraConfigSecrets(context.Background(), extraMap); err != nil {
						slog.Warn("resolve extra_config failed", "name", p.Name, "error", err)
					} else {
						p.ExtraConfig, _ = json.Marshal(extraMap)
					}
				}
			}
		}

		timeout := 300 * time.Second
		prov, err := provider.NewFromModel(&p, timeout)
		if err != nil {
			slog.Warn("skipping unsupported provider", "name", p.Name, "adapter_type", p.AdapterType, "error", err)
			continue
		}
		// VCR recording: wrap in RecordingProvider when extra_config.record=true.
		// store=nil is fine — RecordingProvider.activeStore() reads globalFixtureStore
		// dynamically, which is set later in FullSetup.
		if provider.IsRecordEnabled(p.ExtraConfig) {
			prov = provider.NewRecordingProvider(prov, p.Name, nil)
			slog.Info("recording enabled at startup", "name", p.Name)
		}
		registry.Register(p.Name, prov)
		slog.Info("registered provider", "name", p.Name, "adapter_type", p.AdapterType)
	}
}

func InitRedis(cfg *config.RedisConfig) *redis.Client {
	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr(),
		Password:     cfg.Password,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		MinIdleConns: cfg.MinIdleConns,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		slog.Warn("redis ping failed at startup — will retry at runtime", "addr", cfg.Addr(), "error", err)
	} else {
		slog.Info("redis connected", "addr", cfg.Addr())
	}
	return rdb
}

func ensureAdminUser(db *gorm.DB, cfg *config.AdminConfig) {
	var count int64
	db.Model(&model.User{}).Where("role_id = (SELECT id FROM roles WHERE name = ?)", model.RoleAdmin).Count(&count)
	if count > 0 {
		return
	}
	var adminRole model.Role
	if err := db.Where("name = ?", model.RoleAdmin).First(&adminRole).Error; err != nil {
		slog.Warn("admin role not found, skipping admin user seed", "error", err)
		return
	}
	user := &model.User{
		Username:          cfg.Username,
		PasswordHash:      cfg.PasswordHash,
		DisplayName:       "Administrator",
		RoleID:            adminRole.ID,
		Status:            1,
		ForcePasswordChange: true,
	}
	if err := db.Create(user).Error; err != nil {
		slog.Warn("failed to seed admin user", "error", err)
	} else {
		slog.Info("seeded default admin user", "username", cfg.Username)
	}
}

func syncAdminPermissions(db *gorm.DB) {
	var adminRole model.Role
	if err := db.Where("name = ?", model.RoleAdmin).First(&adminRole).Error; err != nil {
		return
	}
	var existingActions []string
	db.Model(&model.RolePermission{}).Where("role_id = ?", adminRole.ID).Pluck("action", &existingActions)
	existing := make(map[string]bool, len(existingActions))
	for _, a := range existingActions {
		existing[a] = true
	}
	for action := range model.ValidActions {
		if !existing[action] {
			db.Create(&model.RolePermission{RoleID: adminRole.ID, Action: action})
		}
	}
}
