package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	// Registers /debug/pprof/* handlers on http.DefaultServeMux; exposed ONLY
	// behind CL_PPROF=1 + gateway auth (see the gated route below).
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"bufio"
	"net"

	"github.com/crosslink/internal/admin"
	"github.com/crosslink/internal/billingintegrity"
	"github.com/crosslink/internal/apidoc"
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
	"github.com/crosslink/internal/settings"
	"github.com/crosslink/internal/version"
	"github.com/crosslink/internal/worker"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

func FullSetup(cfg *config.Config, db *gorm.DB, rdb *redis.Client, ext *Extensions, dia dialect.Dialect) {
	// Rig/ops forensics switch (CL_LOG_LEVEL=debug): lowers the default
	// slog logger to Debug so settlement/probe traces are emitted. Default
	// (unset/any other value) keeps Info — zero production noise.
	if os.Getenv("CL_LOG_LEVEL") == "debug" {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
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

	// SSRF internal allowlist (B5: hot-reloadable via the admin page).
	// Effective policy precedence: page/DB override > env
	// CL_GATEWAY_INTERNAL_ALLOW_CIDRS > YAML gateway.internal_allow_cidrs >
	// strict default. Cold start is fail-closed: a DB read failure or an
	// invalid stored row publishes the STRICT policy (never the possibly
	// wider env/YAML value) and the fault is surfaced via the status API.
	// The service is created before anything dials so no outbound request
	// can race the initial publish; the refresh/broadcast loop starts once
	// the app context exists (see below).
	ssrfAllowlistSvc := service.NewSsrfAllowlistService(db, rdb, cfg.Gateway.InternalAllowCIDRs, cfg.Gateway.InternalAllowCIDRsFromEnv)
	ssrfAllowlistSvc.LoadColdStart(context.Background())

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

	// Runtime operational config: system_settings row > yaml/env baseline.
	// Constructed after buildSecrets (needs the enc store for smtp_password)
	// and before route registration (middleware closures capture it).
	settingsProvider := settings.NewProvider(db, cfg, secrets.EncStore)
	// The MCP service is constructed in main (before FullSetup) — hand it the
	// provider via the extension hook (same pattern as MCPEncSetter).
	if ext.MCPSettingsSetter != nil {
		ext.MCPSettingsSetter(settingsProvider)
	}

	// Config doctor startup summary: log danger/warn check ids so CLI/docker
	// users get the same signal as the dashboard panel. Read-only, never fatal.
	admin.LogReadinessSummary(db, cfg)

	// Repositories
	repos := repository.ProvideRepos(db)

	// Permission cache for RBAC
	permCache := middleware.NewPermissionCache(repos.RoleRepo)
	if err := permCache.Load(); err != nil {
		slog.Warn("failed to load permission cache", "error", err)
	}

	// Pin the statistics timezone so day buckets and query windows agree with
	// the DB session timezone set in main. The setup-wizard DB row
	// (stats_timezone) wins over database.timezone — same precedence main.go
	// applies when building the DSN.
	statsTZ := admin.LoadStatsTimezone(db)
	if statsTZ == "" {
		statsTZ = cfg.Database.Timezone
	}
	admin.SetStatsTimezone(statsTZ)

	// Services
	cacheRdb := InitCacheRedis(cfg.Cache.Redis)
	svcs := service.ProvideServices(repos, rdb, cacheRdb, db, &cfg.Cache, cryptoProvider, cfg.DataLens, dia, cfg.Gateway.BudgetMarkerTTL)
	guardrailSvc := guardrail.NewGuardrailService(db, rdb)

	// Seed the default credential_detection baseline rule (zero-config safety
	// floor). Idempotent; non-fatal — guardrails are non-critical, a seed error
	// must not block startup.
	if err := guardrail.SeedDefaultRules(db); err != nil {
		slog.Warn("failed to seed default guardrail rules", "error", err)
	} else {
		slog.Info("guardrail default rules seeded")
	}

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
	// Cross-instance rules/settings reload: subscribe for the process
	// lifetime. Convergence on other instances is TTL-bounded best-effort,
	// not atomic; staleness is observable via
	// cl_guardrail_rules_last_loaded_success_timestamp_seconds.
	guardrailSvc.StartReloadSubscription(context.Background())
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
	// MUST run before ProvideInfrastructure: boot-registered providers build
	// their stream transports here, and transports snapshot the per-host
	// connection cap at creation time. Calling it later (as this used to)
	// silently left every boot provider on the default cap — the configured
	// gateway.upstream_max_conns_per_host only ever applied to providers
	// created later via admin CRUD (found via the 1500-stream capacity run:
	// upstream peak pinned at 200 despite an env override).
	provider.SetMaxConnsPerHost(cfg.Gateway.UpstreamMaxConnsPerHost)
	// Idle-pool sizing is a deployment property (default 20/host, 400 total
	// churned ~111 conns/s at 1200 concurrent streams and saturated the
	// Windows ephemeral range in ~100s — r12c/r13). Raise toward the steady
	// concurrent-stream count so released conns are reused, not redialed.
	provider.SetMaxIdleConns(cfg.Gateway.UpstreamMaxIdleConnsPerHost, cfg.Gateway.UpstreamMaxIdleConns)
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
	// Background refresh: keep the runtime settings snapshot in sync across
	// instances (admin writes hot-apply locally via RefreshNow).
	go settingsProvider.RunRefreshLoop(appCtx, 30*time.Second)
	// SSRF allowlist (B5): bounded-TTL pull plus Redis invalidation broadcast
	// keeps every instance's policy converging after a page change.
	go ssrfAllowlistSvc.Start(appCtx)
	// Background refresh: keep the error-classification rule table hot-reloaded
	go infra.Classifier.RunRefreshLoop(appCtx)

	// Circuit prober: while any circuit is open, probe the upstream cheaply
	// every interval and close it as soon as recovery is verified. Enabled by
	// default; the interval/enabled settings take effect on restart.
	if rcProbe := admin.LoadResilienceConfig(db); rcProbe.CircuitProbeEnabled != 0 {
		prober := service.NewCircuitProber(
			infra.Health, infra.Registry,
			repos.ProviderRepo, repos.ProviderModelCRUDRepo,
			secrets.SecretResolver,
			time.Duration(rcProbe.CircuitProbeInterval)*time.Second,
			cfg.Gateway.CircuitProbePath,
		)
		go prober.Run(appCtx)
	}

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
		Settings:       settingsProvider,
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
	videoTaskSvc.SetBudgetSvc(svcs.BudgetSvc) // completion-time budget reporting
	ext.Deps.VideoTaskSvc = videoTaskSvc      // expose to commercial overlay
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

	// Token-estimation calibration (context analysis); nil redis -> memory-only
	calibSvc := service.NewCalibrationService(rdb)
	anthropicHandler.SetCalibration(calibSvc)
	openaiHandler.SetCalibration(calibSvc)

	usageWorkers := worker.NewPool("usage", cfg.Usage.Workers, cfg.Usage.QueueSize, cfg.Usage.OverflowCap)
	handler.SetUsageWorkers(usageWorkers)
	// Disk spill for billing rows the pool cannot accept (queue + overflow
	// full): rows persist as JSONL and replay once the pool recovers. This is
	// the durability path that keeps usage_logs (the budget-calibration
	// ground truth) complete through DB degradation.
	usageSpool := handler.NewUsageSpool(cfg.Usage.SpoolDir, cfg.Usage.SpoolMaxBytes)
	handler.SetUsageSpool(usageSpool)
	// Replay flushes rounds DIRECTLY to the DB (one multi-values INSERT per
	// round — per-row commits cost one fsync round trip each and capped drain
	// at ~10 rows/s under live load) with ack semantics: a row leaves the
	// spool only after the batch returns nil. Bypassing the pool here is
	// deliberate — enqueue-success is not a DB ack.
	go handler.RunUsageSpoolReplay(appCtx, usageSpool, 30*time.Second, func(ctx context.Context, entries []*service.UsageEntry) error {
		return svcs.UsageSvc.LogBatchChecked(ctx, entries)
	})
	modelsHandler := handler.NewModelsHandler(db)

	// Whole-stream duration cap for SSE (0 = uncapped; see server.stream_max_duration)
	service.SetStreamMaxDuration(cfg.Server.StreamMaxDuration)
	// Stalled-stream cap: close an established SSE stream whose upstream went
	// silent (0 = disabled; see server.stream_idle_timeout).
	provider.SetStreamIdleTimeout(cfg.Server.StreamIdleTimeout)
	// Non-streaming per-attempt cap (was a hardcoded 30s that failed every
	// long reasoning generation as a 502; see gateway.provider_timeout).
	service.SetPerProviderTimeout(cfg.Gateway.ProviderTimeout)
	// Per-host upstream connection cap (gateway.upstream_max_conns_per_host).
	// Only affects transports created after this point (provider registry
	if cfg.Server.WriteTimeout > 0 {
		slog.Warn("server.write_timeout is not applied: a global http.Server WriteTimeout would kill long SSE streams mid-flight. Use server.stream_max_duration / stream_idle_timeout to bound streams.")
	}

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
	// CORS allowlist from runtime settings (DB row > cors.allowed_origins): admin
	// edits hot-apply without restart.
	r.Use(middleware.CORSFromProvider(settingsProvider))
	r.Use(middleware.Logger())
	r.Use(middleware.Metrics())

	// Health endpoint (no auth)
	r.GET("/health", healthCheck(db))
	// /livez is process-liveness ONLY: always 200 while the process can serve
	// HTTP. Liveness probes MUST use this — pointing them at /health turns a
	// fleet-wide DB outage into a restart storm (pods are fine; the DB is
	// not). Readiness keeps /health (DB ping) so dependency failures DO
	// drain traffic from the pod. Redis is deliberately not probed: the
	// gateway serves fail-open when Redis is down (rate-limit semantics,
	// see D-020).
	r.GET("/livez", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "live"})
	})

	// Public OpenAPI spec — for Postman/codegen import. No auth (published docs).
	r.GET("/openapi.json", handler.OpenAPIHandler)

	// Prometheus metrics endpoint (requires gateway auth key)
	r.GET("/metrics", middleware.GatewayMetricsAuth(cfg.Gateway.AuthKey), gin.WrapH(promhttp.Handler()))

	// Ops diagnostics, env-gated (CL_PPROF=1): goroutine/heap dumps for live
	// incident debugging. Off by default; when enabled it MUST stay behind the
	// same auth as /metrics. pprofHandlers (imported above, blank) registers
	// the /debug/pprof/* handlers on http.DefaultServeMux.
	if os.Getenv("CL_PPROF") == "1" {
		auth := middleware.GatewayMetricsAuth(cfg.Gateway.AuthKey)
		r.GET("/debug/pprof/*any", auth, gin.WrapH(http.DefaultServeMux))
	}

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
		DB:               db,
		RDB:              rdb,
		Repos:            repos,
		Svcs:             svcs,
		Resolver:         infra.Resolver,
		Registry:         infra.Registry,
		Health:           infra.Health,
		RetryBudget:      infra.RetryBudget,
		CacheSvc:         svcs.CacheSvc,
		SecretResolver:   secrets.SecretResolver,
		EncStore:         secrets.EncStore,
		DebugStore:       debugStore,
		Crypto:           cryptoProvider,
		Config:           cfg,
		Settings:         settingsProvider,
		AuditSvc:         ext.Deps.AuditSvc,
		TemplateRegistry: templateRegistry,
		TemplateSync:     templateSync,
		SsrfAllowlist:    ssrfAllowlistSvc,
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
	loginReadiness := admin.NewReadinessChecker(db, cfg)
	loginReadiness.SetSettingsProvider(settingsProvider)
	// Hot-swap the captcha config (runtime settings): enable/disable, trust
	// window, slider geometry. The HMAC secret is not swappable — trust
	// cookies survive config changes.
	settingsProvider.Subscribe(func(s *settings.Snapshot) {
		gateCfg, sliderCfg := captchaConfigsFrom(s.Captcha)
		captchaGate.ApplyConfig(gateCfg, sliderCfg)
	})
	// Captcha issue: pre-auth, image generation is ~2.2ms CPU each — a bot
	// farm can eat 10-20 cores issuing captchas even though failed LOGINS
	// are capped. U-16: per-IP issue limiter (30/min), same limiter family
	// as login.
	r.GET("/admin/api/auth/captcha/issue", middleware.LoginRateLimit(rdb, 30, time.Minute), admin.CaptchaIssueHandler(captchaGate))
	r.POST("/admin/api/auth/login", middleware.LoginRateLimit(rdb, 10, 15*time.Minute), admin.LoginHandler(repos.UserRepo, repos.TeamRepo, repos.RoleRepo, repos.OrgRepo, cfg.Admin, ext.Deps.AuditSvc, cryptoProvider, captchaGate, loginReadiness))
	r.POST("/admin/api/auth/logout", admin.LogoutHandler())

	// Lightweight auth endpoints (JWT required, exempt from rate limit)
	authGroup := r.Group("/admin/api")
	authGroup.Use(admin.JWTAuthMiddleware(cfg.Admin, db, cryptoProvider))
	authGroup.Use(middleware.OrgResolve())
	{
		authGroup.GET("/version", func(c *gin.Context) {
			// Top-level fields kept for backward compatibility (additive-only:
			// `api` was added without moving them — see docs/api-versioning.md).
			c.JSON(200, gin.H{
				"version": version.Version,
				"commit":  version.Commit,
				"date":    version.Date,
				// API surface versions — independent of the build version so
				// clients can pin behavior, not releases. Gateway, admin API and
				// MCP each carry their own version; see docs/api-versioning.md.
				"api": gin.H{
					"gateway": "v1",
					"admin":   "v1",
					"mcp":     "1.0",
				},
			})
		})
		authGroup.GET("/deprecations", func(c *gin.Context) {
			c.JSON(200, gin.H{"data": apidoc.ActiveDeprecations})
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
		adminGroup.POST("/keys/import", middleware.RequireAction(permCache, "key:import"), handlers.Key.Import)

		// Usage
		adminGroup.GET("/usage", middleware.RequireAction(permCache, "usage:list"), handlers.Usage.List)

		adminGroup.GET("/usage/stats", middleware.RequireAction(permCache, "usage:stats"), handlers.Usage.Stats)
		adminGroup.GET("/usage/daily", middleware.RequireAction(permCache, "usage:list"), handlers.Usage.DailyTrend)
		adminGroup.GET("/usage/models", middleware.RequireAction(permCache, "usage:list"), handlers.Usage.ModelDistribution)
		adminGroup.GET("/usage/templates", middleware.RequireAction(permCache, "usage:list"), handlers.Usage.TemplateStats)
		adminGroup.GET("/usage/reconciliation/export", middleware.RequireAction(permCache, "usage:stats"), handlers.Usage.ReconciliationExport)
		adminGroup.GET("/usage/team-stats", middleware.RequireAction(permCache, "usage:stats"), handlers.Usage.TeamStats)
		adminGroup.GET("/usage/:id", middleware.RequireAction(permCache, "usage:list"), handlers.Usage.Get)
		adminGroup.GET("/routing/stats", middleware.RequireAction(permCache, "routing:stats"), handlers.Routing.Stats)

		// Guardrails (Community baseline)
		guardrailHandler := admin.NewGuardrailHandler(db, guardrailSvc, ext.Deps.AuditSvc)
		adminGroup.GET("/guardrails", middleware.RequireAction(permCache, "guardrail:list"), guardrailHandler.List)
		adminGroup.POST("/guardrails", middleware.RequireAction(permCache, "guardrail:create"), guardrailHandler.Create)
		adminGroup.PUT("/guardrails/:id", middleware.RequireAction(permCache, "guardrail:update"), guardrailHandler.Update)
		adminGroup.DELETE("/guardrails/:id", middleware.RequireAction(permCache, "guardrail:delete"), guardrailHandler.Delete)
		// M3-A versioning: history (sanitized) + rollback-as-new-version, same
		// RBAC actions as rule list/update.
		adminGroup.GET("/guardrails/versions", middleware.RequireAction(permCache, "guardrail:list"), guardrailHandler.Versions)
		adminGroup.POST("/guardrails/:id/rollback", middleware.RequireAction(permCache, "guardrail:update"), guardrailHandler.Rollback)
		adminGroup.POST("/guardrails/:id/test", middleware.RequireAction(permCache, "guardrail:test"), guardrailHandler.Test)
		adminGroup.GET("/guardrails/stats", middleware.RequireAction(permCache, "guardrail:list"), guardrailHandler.Stats)
		adminGroup.GET("/guardrails/config", middleware.RequireAction(permCache, "guardrail:list"), guardrailHandler.GetConfig)
		adminGroup.PUT("/guardrails/config", middleware.RequireAction(permCache, "guardrail:update"), guardrailHandler.UpdateConfig)

		// Playground (community-core) — interactive testing for all modes
		// (chat/stream/image/tts/stt/video/responses). Multimodal modes
		// capability-probe providers via type assertion.
		playgroundHandler := admin.NewPlaygroundHandler(infra.Resolver, svcs.UsageSvc, svcs.LatencySvc, svcs.ActiveTracker, infra.RetryBudget, guardrailSvc, videoTaskSvc)
		pg := adminGroup.Group("/playground")
		// Playground traffic consumes team/org budgets (identity from JWT
		// context: team_id/org_id). Key-level budget/calls don't apply — no API key.
		pg.Use(middleware.AdminBudgetCheck(svcs.BudgetSvc, teamCache, orgCache))
		pg.Use(middleware.AdminReportBudgetUsage(svcs.BudgetSvc, svcs.BudgetAlertSvc, teamCache, orgCache))
		pg.POST("/chat", middleware.RequireAction(permCache, "playground:use"), playgroundHandler.Chat)
		pg.POST("/stream", middleware.RequireAction(permCache, "playground:use"), playgroundHandler.StreamChat)
		pg.POST("/image", middleware.RequireAction(permCache, "playground:use"), playgroundHandler.ImageGenerate)
		pg.POST("/tts", middleware.RequireAction(permCache, "playground:use"), playgroundHandler.TextToSpeech)
		pg.POST("/transcribe", middleware.RequireAction(permCache, "playground:use"), playgroundHandler.Transcribe)
		pg.POST("/translate", middleware.RequireAction(permCache, "playground:use"), playgroundHandler.Translate)
		pg.POST("/video", middleware.RequireAction(permCache, "playground:use"), playgroundHandler.VideoGenerate)
		pg.GET("/video/:taskId", middleware.RequireAction(permCache, "playground:use"), playgroundHandler.GetVideoStatus)
		pg.POST("/responses", middleware.RequireAction(permCache, "playground:use"), playgroundHandler.Responses)

		// Prompt templates (context engineering). Super-admin only (AdminExclusiveActions).
		adminGroup.GET("/templates", middleware.RequireAction(permCache, "template:list"), handlers.Templates.List)
		adminGroup.POST("/templates", middleware.RequireAction(permCache, "template:create"), handlers.Templates.Create)
		adminGroup.GET("/templates/:id", middleware.RequireAction(permCache, "template:list"), handlers.Templates.Get)
		adminGroup.PUT("/templates/:id", middleware.RequireAction(permCache, "template:update"), handlers.Templates.Update)
		adminGroup.DELETE("/templates/:id", middleware.RequireAction(permCache, "template:delete"), handlers.Templates.Delete)
		adminGroup.POST("/templates/:id/preview", middleware.RequireAction(permCache, "template:list"), handlers.Templates.Preview)

		// System info (self-service, no RequireAction)
		adminGroup.GET("/system/info", handlers.System.Info)
		// Config doctor: readiness report (self-service, no RequireAction —
		// every viewer should see deployment health, like /system/info).
		adminGroup.GET("/system/readiness", handlers.Readiness.Report)
		// Runtime operational config: DB-backed settings with hot-apply
		// (Community-owned — replaces the frontend's dependency on the
		// Pro-only /system/settings aggregate).
		adminGroup.GET("/system/config", middleware.RequireAction(permCache, "system:view"), handlers.System.GetConfig)
		adminGroup.PUT("/system/config", middleware.RequireAction(permCache, "system:update"), handlers.System.UpdateConfig)
		// SSRF internal allowlist (B5): hot-reload without restart. Writes are
		// platform-admin only (RequireAction + handler-side IsAdmin); reads
		// follow the same system:view gate as the rest of system config.
		adminGroup.GET("/system/ssrf-allowlist", middleware.RequireAction(permCache, "system:view"), handlers.SsrfAllowlist.Get)
		adminGroup.PUT("/system/ssrf-allowlist", middleware.RequireAction(permCache, "system:update"), handlers.SsrfAllowlist.Put)
		adminGroup.DELETE("/system/ssrf-allowlist", middleware.RequireAction(permCache, "system:update"), handlers.SsrfAllowlist.Delete)
		// Sensitive-op re-authentication (password → short-lived single-use
		// token, user-bound, rate-limited; consumed by PUT/DELETE above).
		adminGroup.POST("/system/ssrf-allowlist/reauth", middleware.RequireAction(permCache, "system:update"), handlers.SsrfAllowlist.Reauth)
		// First-run setup wizard persistence (system_settings KV rows).
		adminGroup.POST("/system/setup/apply", middleware.RequireAction(permCache, "system:update"), handlers.Setup.Apply)
		adminGroup.POST("/system/setup/skip", middleware.RequireAction(permCache, "system:update"), handlers.Setup.Skip)
		adminGroup.POST("/system/password", middleware.RequireAction(permCache, "system:password"), handlers.System.ChangePassword)
		// Content-log toggle — community-core setting; the aggregate
		// /system/settings endpoint stays in the Pro overlay.
		adminGroup.GET("/system/content-log", middleware.RequireAction(permCache, "system:view"), handlers.System.GetContentLog)
		adminGroup.PUT("/system/content-log", middleware.RequireAction(permCache, "system:update"), handlers.System.UpdateContentLog)

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

		// PAT management (self-service scoped tokens)
		patSvc := service.NewPatService(repos.PatTokenRepo)
		patHandler := admin.NewPATAdminHandler(patSvc, repos.PatTokenRepo, repos.RoleRepo, ext.Deps.AuditSvc)
		adminGroup.GET("/pats", middleware.RequireAction(permCache, "pat:manage"), patHandler.List)
		adminGroup.POST("/pats", middleware.RequireAction(permCache, "pat:manage"), patHandler.Create)
		adminGroup.DELETE("/pats/:id", middleware.RequireAction(permCache, "pat:manage"), patHandler.Revoke)

		// PAT read-only routes — explicit allowlist group (design: default-deny).
		// No JWT/CSRF/OrgResolve: PATAuthMiddleware resolves identity from the
		// token itself (org_id=0 for Phase 1 admin-only); header-only auth
		// blocks CSRF.
		//
		// Rate-limit ordering note: the group-level limiter runs before the
		// route-level PATAuthMiddleware, so no user_id is in the context and
		// the limit key falls back to ClientIP. This is accepted: per-IP
		// limiting suits single-IP agents and avoids coupling the limiter to
		// auth middleware order.
		patReadHandler := admin.NewPATReadHandler(admin.PATReadDeps{
			KeyLister:   svcs.KeySvc,
			UsageSummer: &admin.GormUsageSummer{DB: db},
			UsageAgg:    &admin.GormUsageAggregator{DB: db},
			BudgetSpent: svcs.BudgetSvc,
			Health:      infra.Health,
			Version:     version.Version,
		})
		patGroup := r.Group("/admin/api/pat")
		patGroup.Use(middleware.AdminRateLimit(rdb, 60, time.Minute, "pat:"))
		// Audit runs after PATAuthMiddleware (route-level) in the chain, so
		// pat_id is already in the context when its c.Next() returns.
		// Pass a bare nil when AuditSvc is absent (Community): a nil
		// *service.AuditService inside the AuditLogger interface is not == nil.
		var patAuditSvc middleware.AuditLogger
		if ext.Deps.AuditSvc != nil {
			patAuditSvc = ext.Deps.AuditSvc
		}
		patGroup.Use(middleware.PATAudit(patAuditSvc))
		{
			patGroup.GET("/keys", middleware.PATAuthMiddleware(patSvc, repos.UserRepo, "key:list"), middleware.RequireAction(permCache, "key:list"), patReadHandler.Keys)
			patGroup.GET("/usage/summary", middleware.PATAuthMiddleware(patSvc, repos.UserRepo, "usage:list"), middleware.RequireAction(permCache, "usage:list"), patReadHandler.Usage)
			patGroup.GET("/budgets/status", middleware.PATAuthMiddleware(patSvc, repos.UserRepo, "budget:read"), middleware.RequireAction(permCache, "budget:read"), patReadHandler.Budgets)
			patGroup.GET("/health", middleware.PATAuthMiddleware(patSvc, repos.UserRepo, "health:read"), middleware.RequireAction(permCache, "health:read"), patReadHandler.Health)
		}

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
	// API version stamp + deprecation headers (X-API-Version, Deprecation,
	// Sunset — see docs/api-versioning.md). Mounted first so even aborted
	// chain responses carry the headers.
	gwGroup.Use(middleware.APIVersion("v1", func() []apidoc.Deprecation {
		return apidoc.ActiveDeprecations
	}))
	if cfg.Gateway.InflightBodyBudgetBytes <= 0 {
		// U-23: 0 means EXPLICITLY DISABLED (no aggregate inbound-body cap) —
		// make that loud and observable instead of silent: the metric reports
		// 0 and this warning is greppable. Default stays 0 (a numeric default
		// is a per-host memory decision; see deployment docs).
		slog.Warn("gateway.inflight_body_budget_bytes=0: in-flight request-body budget DISABLED — aggregate inbound memory is unbounded up to concurrency × per-request caps")
	}
	gwGroup.Use(middleware.ReadBodyWithBudget(10<<20,
		middleware.NewInflightBodyBudget(cfg.Gateway.InflightBodyBudgetBytes),
		cfg.Gateway.InflightMultipartReserveBytes))
	gwGroup.Use(middleware.ContextAssembler(templateRegistry, ext.AssemblerHook))
	gwGroup.Use(debug.Middleware(debugStore))
	gwGroup.Use(middleware.UsageLog(svcs.UsageSvc))
	gwGroup.Use(middleware.AuthFailureLimit(rdb, 20, 15*time.Minute, ""))
	gwGroup.Use(middleware.Auth(cfg.Gateway.AuthKey, svcs.KeySvc, rdb, ext.IPPolicy))
	gwGroup.Use(middleware.RequireModel())
	gwGroup.Use(middleware.OrgResolve())
	gwGroup.Use(middleware.GuardrailsRequest(guardrailSvc))
	gwGroup.Use(middleware.Cache(svcs.CacheSvc, cryptoProvider, guardrailSvc))
	gwGroup.Use(middleware.RateLimitFromProvider(rdb, settingsProvider, teamCache, orgCache))
	// BudgetCheck before TPMLimit: a budget-rejected request must not make a
	// TPM reservation (the aborting chain never reaches ReportTokens).
	gwGroup.Use(middleware.BudgetCheck(svcs.BudgetSvc, teamCache, orgCache))
	gwGroup.Use(middleware.TPMLimitFromProvider(rdb, settingsProvider, teamCache, orgCache))
	gwGroup.Use(middleware.ReportTokens(rdb, orgCache))
	gwGroup.Use(middleware.ReportBudgetUsage(svcs.BudgetSvc, svcs.BudgetAlertSvc, teamCache, orgCache))
	// RoutingStats (per-minute Redis hashes) removed: it was write-only —
	// /admin/routing/stats reads usage_logs directly. Reintroduce with a reader
	// if minute-level routing telemetry is needed.
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
	usageGroup.Use(middleware.APIVersion("v1", func() []apidoc.Deprecation {
		return apidoc.ActiveDeprecations
	}))
	usageGroup.Use(middleware.Auth(cfg.Gateway.AuthKey, svcs.KeySvc, rdb, ext.IPPolicy))
	// U-17: this group authenticates keys with the same DB-backed KeyService
	// (no negative cache) — random-key scanning here was an unbounded DB
	// point-query path. Share the gateway group's per-IP auth-failure budget
	// (same prefix/params) so scanning EITHER surface throttles the IP.
	usageGroup.Use(middleware.AuthFailureLimit(rdb, 20, 15*time.Minute, ""))
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
	readHeaderTimeout := 15 * time.Second
	if cfg.Server.ReadTimeout > 0 && cfg.Server.ReadTimeout < readHeaderTimeout {
		readHeaderTimeout = cfg.Server.ReadTimeout
	}
	// Per-WRITE deadline for response bodies (acceptance blocker 3): with
	// WriteTimeout=0 a client that keeps the connection open but never reads
	// blocks the handler's SSE Write/Flush FOREVER — a request-context
	// deadline cannot unblock a parked TCP write. The wrapper below arms a
	// write deadline before every Write/Flush on the REAL connection
	// (http.ResponseController), so a stalled consumer surfaces as a write
	// error and the handler, admission slot, upstream stream and billing
	// resources all unwind. Slow-but-alive readers are unaffected: each
	// individual write completes as the socket drains. 0 disables (legacy).
	streamWriteTimeout := cfg.Server.StreamWriteTimeout
	srv := &http.Server{
		Addr:              addr,
		Handler:           sseDeadlineWrap(r, streamWriteTimeout),
		ReadTimeout:       cfg.Server.ReadTimeout,
		ReadHeaderTimeout: readHeaderTimeout, // slowloris header holding must not occupy a conn for the full ReadTimeout
		IdleTimeout:       90 * time.Second,  // keep-alive idling must not be bounded by ReadTimeout (30s default churns connections)
		WriteTimeout:      0,                 // intentionally 0 — the per-write deadline above is the bounded-write policy
	}

	// Cleanup expired key hashes every 10 minutes
	// Budget calibration every hour
	go svcs.BudgetCalSvc.Run(appCtx)

	// U-13: renew the mcp_tool_call_logs partition horizon (start + daily).
	// The maintenance method existed but had ZERO production callers — from
	// 2027-01 inserts into MySQL would fail (or pile into p_future) with no
	// warning. dialects: mysql reorganizes the p_future tail; pg/kingbase
	// add partitions idempotently; sqlite's implementation is archive
	// cleanup (harmless to call).
	go func() {
		ensure := func() {
			ctx, cancel := context.WithTimeout(appCtx, 2*time.Minute)
			defer cancel()
			if err := dia.EnsureMonthlyPartitions(ctx, db, "mcp_tool_call_logs", 6); err != nil {
				slog.Error("partition renewal failed (mcp_tool_call_logs) — inserts may fail when the horizon is reached", "error", err)
			}
		}
		ensure()
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-appCtx.Done():
				return
			case <-t.C:
				ensure()
			}
		}
	}()
	// DataLens aggregation background goroutine
	if cfg.DataLens.Enabled {
		// U-11: the aggregation SQL is Postgres-dialect ($N placeholders,
		// expression-target ON CONFLICT ... EXCLUDED). MySQL cannot parse it
		// and SQLite has no expression conflict targets — refuse to start the
		// loop there instead of erroring every cycle.
		if n := dia.Name(); n != "postgres" && n != "kingbase" {
			slog.Error("DataLens aggregation requires PostgreSQL/Kingbase (aggregation SQL uses $N placeholders and expression ON CONFLICT targets); DataLens disabled on this dialect", "dialect", n)
		} else {
			if dia.PartitionSupport() == dialect.PartitionNative {
				var relkind string
				db.Raw("SELECT relkind FROM pg_class WHERE relname = 'usage_logs'").Scan(&relkind)
				if relkind != "p" {
					slog.Warn("usage_logs is not partitioned. Run 'crosslink migrate-partition' to enable partitioning. DataLens aggregation will use unpartitioned table (slower).")
				}
			}
			// Retention day counts hot-reload via runtime settings (interval /
			// lookback / enabled stay boot-pinned, labeled restart-required).
			svcs.DataLensAggSvc.SetSettingsProvider(settingsProvider)
			go svcs.DataLensAggSvc.Run(appCtx)
		}
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
		// Surface effective rate-limit settings so a config file picked up from
		// an unexpected location can't enable limits silently (429 incident).
		slog.Info("rate limit config",
			"rpm", cfg.RateLimit.RPM,
			"tpm", cfg.RateLimit.TPM,
			"tpm_reservation", cfg.RateLimit.Reservation,
			"fail_closed", cfg.RateLimit.FailClosed)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("failed to start server", "error", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("shutting down server...", "active_requests", dm.ActiveCount())

	// Phase 1: Drain in-flight gateway streams (server.drain_timeout; the
	// SUM of all three phase budgets must fit the container stop grace).
	drainTimeout := cfg.Server.DrainTimeout
	if drainTimeout <= 0 {
		drainTimeout = 60 * time.Second
	}
	remaining := dm.Drain(drainTimeout)
	if remaining > 0 {
		slog.Warn("drain timeout exceeded, forcing shutdown", "remaining", remaining)
	} else {
		slog.Info("all in-flight requests completed during drain")
	}

	// Phase 2: Stop accepting new connections, wait for remaining
	shutdownBudget := cfg.Server.ShutdownTimeout
	if shutdownBudget <= 0 {
		shutdownBudget = 10 * time.Second
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server forced to shutdown", "error", err)
	}

	// Phase 3: Flush usage worker pool
	flushBudget := cfg.Server.WorkerFlushTimeout
	if flushBudget <= 0 {
		flushBudget = 10 * time.Second
	}
	workerCtx, workerCancel := context.WithTimeout(context.Background(), flushBudget)
	defer workerCancel()
	usageWorkers.Shutdown(workerCtx)

	// Phase 4: Cancel background goroutines
	videoTaskSvc.Close()
	appCancel()
	calibSvc.Stop() // flush calibration EMA state and stop its loop

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

// InitCacheRedis returns the response-cache Redis client: a dedicated
// instance when cache.redis is configured (cache keys tolerate eviction —
// allkeys-lru — while quota/budget/idempotency keys on the main instance must
// not be evicted; a separate maxmemory domain requires a separate instance,
// DB numbers share maxmemory/eviction), otherwise nil (share the main one).
func InitCacheRedis(cr *config.RedisConfig) *redis.Client {
	if cr == nil || cr.Host == "" {
		return nil
	}
	addr := fmt.Sprintf("%s:%d", cr.Host, cr.Port)
	cacheRdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     cr.Password,
		DB:           cr.DB,
		PoolSize:     cr.PoolSize,
		MinIdleConns: cr.MinIdleConns,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond,
	})
	cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ccancel()
	if err := cacheRdb.Ping(cctx).Err(); err != nil {
		slog.Warn("cache redis ping failed at startup — will retry at runtime", "addr", addr, "error", err)
	} else {
		slog.Info("cache redis connected (dedicated instance)", "addr", addr)
	}
	return cacheRdb
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
		Username:            cfg.Username,
		PasswordHash:        cfg.PasswordHash,
		DisplayName:         "Administrator",
		RoleID:              adminRole.ID,
		Status:              1,
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

// sseDeadlineWrap installs the per-write response deadline (acceptance
// blocker 3). It wraps OUTSIDE the gin engine so http.ResponseController is
// built from the REAL *http.response (gin's writer boxing hides the conn's
// SetWriteDeadline). Every Write/Flush arms a fresh deadline; a consumer
// that never reads gets a write timeout error instead of parking the handler
// forever. The wrapper must keep implementing http.Flusher and
// http.Hijacker for gin's writer probe.
func sseDeadlineWrap(next http.Handler, timeout time.Duration) http.Handler {
	if timeout <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctl := http.NewResponseController(w)
		dw := &deadlineResponseWriter{ResponseWriter: w, ctl: ctl, timeout: timeout}
		next.ServeHTTP(dw, r)
	})
}

type deadlineResponseWriter struct {
	http.ResponseWriter
	ctl     *http.ResponseController
	timeout time.Duration
}

func (w *deadlineResponseWriter) Write(p []byte) (int, error) {
	// Best-effort arming: controllers over exotic writers may not support
	// deadlines; failing to arm must not break the response itself.
	_ = w.ctl.SetWriteDeadline(time.Now().Add(w.timeout))
	return w.ResponseWriter.Write(p)
}

func (w *deadlineResponseWriter) Flush() {
	_ = w.ctl.SetWriteDeadline(time.Now().Add(w.timeout))
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack delegates so websocket/upgrade paths keep working through the
// wrapper (gin probes http.Hijacker on its writer; a bare embedded interface
// would NOT satisfy that probe). Deadlines do not apply after a successful
// hijack — the conn belongs to the hijacker.
func (w *deadlineResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hj.Hijack()
}

// Unwrap lets http.ResponseController (and middleware built on it) see
// through to the REAL connection instead of treating this wrapper as the
// terminal writer.
func (w *deadlineResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
