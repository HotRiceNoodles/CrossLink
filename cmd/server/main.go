package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/gin-gonic/gin"
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

	dia, err := dialect.New(dialect.DBConfig{
		Driver:     cfg.Database.Driver,
		Host:       cfg.Database.Host,
		Port:       cfg.Database.Port,
		User:       cfg.Database.User,
		Password:   cfg.Database.Password,
		DBName:     cfg.Database.DBName,
		SSLMode:    cfg.Database.SSLMode,
		SQLitePath: cfg.Database.SQLitePath,
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
	mcpRepo := mcp.NewMCPRepo(db)
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
