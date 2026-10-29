package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"strings"

	"github.com/crosslink/internal/app"
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/dialect"
	"github.com/crosslink/internal/license"
	"github.com/crosslink/internal/mcp"
	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/otelsetup"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/secret"
	"github.com/crosslink/internal/version"
	"github.com/gin-gonic/gin"
)

func setupLogger(cfg *config.Config) {
	var level slog.Level
	switch cfg.Logging.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if cfg.Logging.Format == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(handler))
}

func main() {
	// -migrate-only: one-shot schema migration for the controlled Job flow
	// (templates/kubernetes/migrate-job.yaml). Exit contract: 0 = migrations
	// applied (or already up to date — idempotent), non-zero = failure. The
	// server is never started in this mode.
	migrateOnly := flag.Bool("migrate-only", false, "run schema migrations, then exit (0 = success/idempotent no-op, non-zero = failure)")
	flag.Parse()

	cfg, err := config.Load("")
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	setupLogger(cfg)

	// Initialize OpenTelemetry tracing (disabled by default, set CL_OTEL_EXPORTER=stdout to enable)
	otelShutdown, err := otelsetup.InitTracer(context.Background(), "crosslink", version.Version)
	if err != nil {
		slog.Error("failed to init OpenTelemetry", "error", err)
		os.Exit(1)
	}
	defer otelShutdown(context.Background())

	// Effective stats timezone: the setup-wizard DB override (stats_timezone
	// row) wins over database.timezone, else the process local zone. Used both
	// for the DB session timezone (SQL date bucketing) and the admin
	// usage-statistics day boundaries — keeping the two in sync is the fix for
	// "early-morning requests bucketed into the previous day".
	//
	// Only pass a real IANA name to the DSN: time.Local.String() can return
	// the placeholder "Local" (e.g. Windows without zoneinfo), which the
	// database rejects as an invalid TimeZone value. An empty value omits the
	// DSN parameter and the session keeps the server default.
	//
	// Two-phase connect: the timezone rides in the DSN, but the override
	// lives in the DB — so a short-lived preliminary pool (timezone-less)
	// reads it first. Errors (e.g. system_settings not migrated yet on a
	// fresh install) mean "no override". SQLite has no DSN timezone
	// parameter, so it skips the preliminary connection.
	dbTZ := ""
	if cfg.Database.Driver != "sqlite" {
		if tz, err := readStatsTimezoneOverride(cfg); err != nil {
			slog.Warn("failed to read stats_timezone override, using config timezone", "error", err)
		} else {
			dbTZ = tz
		}
	}
	effectiveTZ := resolveEffectiveTimezone(dbTZ, cfg.Database.Timezone)
	if dbTZ != "" && dbTZ != cfg.Database.Timezone && cfg.Database.Timezone != "" {
		slog.Warn("stats_timezone DB override shadows database.timezone", "db", dbTZ, "config", cfg.Database.Timezone)
	}
	dia, err := dialect.New(dialect.DBConfig{
		Driver:          cfg.Database.Driver,
		Host:            cfg.Database.Host,
		Port:            cfg.Database.Port,
		User:            cfg.Database.User,
		Password:        cfg.Database.Password,
		DBName:          cfg.Database.DBName,
		SSLMode:         cfg.Database.SSLMode,
		SQLitePath:      cfg.Database.SQLitePath,
		Timezone:        effectiveTZ,
		MaxOpenConns:    cfg.Database.MaxOpenConns,
		MaxIdleConns:    cfg.Database.MaxIdleConns,
		ConnMaxLifetime: cfg.Database.ConnMaxLifetime,
		ConnMaxIdleTime: cfg.Database.ConnMaxIdleTime,
	})
	if err != nil {
		slog.Error("failed to create dialect", "error", err)
		os.Exit(1)
	}

	db, err := dia.InitDB()
	if err != nil {
		slog.Error("failed to init database", "error", err)
		os.Exit(1)
	}

	// CL_SKIP_MIGRATIONS=1/true: resident pods skip startup migrations — the
	// one-shot migrate Job (see templates/kubernetes/migrate-job.yaml) owns
	// schema changes in that deployment flow, so rolling pods never compete
	// on the migration lock. Unset/other values keep the legacy
	// migrate-at-boot behavior.
	skipMigrations := func() bool {
		v := strings.ToLower(strings.TrimSpace(os.Getenv("CL_SKIP_MIGRATIONS")))
		return v == "1" || v == "true"
	}()
	if skipMigrations && !*migrateOnly {
		slog.Info("startup migrations skipped (CL_SKIP_MIGRATIONS set); the migrate Job owns schema changes")
	} else {
		release, err := dia.AcquireMigrationLock()
		if err != nil {
			slog.Error("failed to acquire migration lock", "error", err)
			os.Exit(1)
		}
		if err := dia.RunMigrations(context.Background()); err != nil {
			release()
			slog.Error("failed to run migrations", "error", err)
			os.Exit(1)
		}
		release()
	}
	if *migrateOnly {
		slog.Info("migrate-only: schema is up to date, exiting", "driver", cfg.Database.Driver)
		os.Exit(0)
	}

	rdb := app.InitRedis(&cfg.Redis)

	cp, err := crypto.NewProvider(cfg.Crypto.Mode)
	if err != nil {
		slog.Error("failed to create crypto provider", "error", err)
		os.Exit(1)
	}

	gate := license.CommunityInit(cfg.License, db, cp)
	defer gate.Stop()
	slog.Info("license tier", "tier", gate.CurrentTier())

	// Initialize MCP Gateway
	mcpRepo := mcp.NewMCPRepo(db, dia)
	mcpRegistry := mcp.NewRegistry()
	mcpSvc := mcp.NewMCPService(mcpRepo, mcpRegistry, cfg.MCP, nil)
	mcpHandler := mcp.NewHandler(mcpSvc)
	mcpAdmin := mcp.NewAdminHandler(mcpSvc, nil)

	if cfg.MCP.Enabled {
		if err := mcpSvc.LoadFromDB(context.Background()); err != nil {
			slog.Warn("failed to load MCP servers", "error", err)
		}
		healthCtx, healthCancel := context.WithCancel(context.Background())
		defer healthCancel()
		mcpSvc.StartHealthChecks(healthCtx)
		mcpSvc.StartLogWorker(healthCtx)
		mcpSvc.StartLogCleanup(healthCtx)
	}

	ext := &app.Extensions{
		Gate:            gate,
		ExtraStrategies: map[router.StrategyName]router.RoutingStrategy{},
		MCPEncSetter: func(encStore *secret.EncryptedDBStore) {
			if encStore != nil {
				mcpSvc.SetEncStore(encStore)
				mcpAdmin.SetEncStore(encStore)
			}
		},
		MCPSettingsSetter: func(p mcp.MCPSettingsProvider) {
			mcpSvc.SetSettingsProvider(p)
		},
		ExtraMCPRoutes: func(mcpGroup *gin.RouterGroup, ext *app.Extensions) {
			var mcpKeyValidator mcp.KeyValidator
			if ext.Deps.KeySvc != nil {
				mcpKeyValidator = ext.Deps.KeySvc
			}
			mcpGroup.Use(mcp.MCPAuth(cfg.Gateway.AuthKey, mcpKeyValidator))
			if cfg.MCP.RateLimitEnabled {
				rpm := cfg.MCP.RateLimitDefaultRPM
				if rpm <= 0 {
					rpm = 60
				}
				mcpGroup.Use(mcp.MCPRateLimit(mcp.NewRateLimiter(rpm)))
			}
			// Agent toolset profiling (Pro overlay seam); nil in Community —
			// the handler then skips observation entirely.
			mcpHandler.SetToolObserver(ext.ToolObserver)
			mcpGroup.POST("/:server", mcpHandler.HandleJSONRPC)
			mcpGroup.GET("/:server", mcpHandler.HandleSSE)
		},
		ExtraRoutes: func(admin *gin.RouterGroup, ext *app.Extensions) {
			admin.GET("/mcp/servers", middleware.RequireAction(ext.Deps.PermCache, "mcp:list"), mcpAdmin.List)
			admin.POST("/mcp/servers", middleware.RequireAction(ext.Deps.PermCache, "mcp:create"), mcpAdmin.Create)
			admin.GET("/mcp/servers/:id", middleware.RequireAction(ext.Deps.PermCache, "mcp:view"), mcpAdmin.Get)
			admin.PUT("/mcp/servers/:id", middleware.RequireAction(ext.Deps.PermCache, "mcp:update"), mcpAdmin.Update)
			admin.DELETE("/mcp/servers/:id", middleware.RequireAction(ext.Deps.PermCache, "mcp:delete"), mcpAdmin.Delete)
			admin.POST("/mcp/servers/:id/test", middleware.RequireAction(ext.Deps.PermCache, "mcp:view"), mcpAdmin.Test)
			admin.GET("/mcp/servers/:id/tools", middleware.RequireAction(ext.Deps.PermCache, "mcp:view"), mcpAdmin.GetTools)
			admin.GET("/mcp/servers/:id/permissions", middleware.RequireAction(ext.Deps.PermCache, "mcp:permission"), mcpAdmin.ListPermissions)
			admin.POST("/mcp/servers/:id/permissions", middleware.RequireAction(ext.Deps.PermCache, "mcp:permission"), mcpAdmin.CreatePermission)
			admin.DELETE("/mcp/servers/:id/permissions/:pid", middleware.RequireAction(ext.Deps.PermCache, "mcp:permission"), mcpAdmin.DeletePermission)
			admin.GET("/mcp/servers/:id/logs", middleware.RequireAction(ext.Deps.PermCache, "mcp:logs"), mcpAdmin.ListLogs)
			admin.GET("/mcp/servers/:id/stats", middleware.RequireAction(ext.Deps.PermCache, "mcp:stats"), mcpAdmin.GetStats)
			admin.GET("/mcp/stats", middleware.RequireAction(ext.Deps.PermCache, "mcp:stats"), mcpAdmin.GetStats)
		},
	}

	app.FullSetup(cfg, db, rdb, ext, dia)
}
