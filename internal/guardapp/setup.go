// Package guardapp assembles the standalone crosslink-guard service: the
// bypass-detection deployment that reuses the Community guardrail stack
// (internal/guardrail) without the gateway proxy surface
// (docs/design/bypass-guard-service.md).
package guardapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/crosslink/internal/app"
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/dialect"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/license"
	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/settings"
	"github.com/crosslink/internal/version"
	"github.com/crosslink/internal/worker"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// App holds the assembled guard services; later M1 steps (pipeline, ingest
// APIs, admin) receive it instead of re-deriving dependencies.
type App struct {
	Cfg       *config.Config
	DB        *gorm.DB
	Dia       dialect.Dialect
	Rdb       *redis.Client
	Gate      *license.Gate
	Settings  *settings.Provider
	Guardrail *guardrail.GuardrailService

	CP        crypto.CryptoProvider
	PermCache *middleware.PermissionCache

	TokenRepo   *repository.GuardTokenRepo
	IngestCache *tokenCache
	Tokens      *IngestTokenService

	Traces   *TraceRecorder
	Pool     *worker.Pool
	Pipeline *Pipeline
	Streams  *StreamStore

	Idem    *idemStore
	Limiter *IngestRateLimiter
}

// SetupLogger mirrors cmd/server's setupLogger (copied rather than shared so
// M1 touches no gateway file; identical semantics: level from logging.level,
// text/JSON handler on stdout).
func SetupLogger(cfg *config.Config) {
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

// Run boots the guard service. With migrateOnly it applies schema migrations
// and returns without serving — the exit contract is identical to the
// gateway's -migrate-only (0 = applied or already up to date).
func Run(migrateOnly bool, configDir string) error {
	cfg, err := config.Load(configDir)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	SetupLogger(cfg)

	// Single-phase connect: unlike cmd/server the guard runs no usage
	// statistics, so it never needs the stats_timezone two-phase dance —
	// database.timezone goes straight into the DSN.
	dia, err := dialect.New(dialect.DBConfig{
		Driver:          cfg.Database.Driver,
		Host:            cfg.Database.Host,
		Port:            cfg.Database.Port,
		User:            cfg.Database.User,
		Password:        cfg.Database.Password,
		DBName:          cfg.Database.DBName,
		SSLMode:         cfg.Database.SSLMode,
		SQLitePath:      cfg.Database.SQLitePath,
		Timezone:        cfg.Database.Timezone,
		MaxOpenConns:    cfg.Database.MaxOpenConns,
		MaxIdleConns:    cfg.Database.MaxIdleConns,
		ConnMaxLifetime: cfg.Database.ConnMaxLifetime,
		ConnMaxIdleTime: cfg.Database.ConnMaxIdleTime,
	})
	if err != nil {
		return fmt.Errorf("create dialect: %w", err)
	}
	db, err := dia.InitDB()
	if err != nil {
		return fmt.Errorf("init database: %w", err)
	}

	// Startup migrations: same execution entry and CL_SKIP_MIGRATIONS /
	// -migrate-only contract as the gateway (see cmd/server/main.go).
	skipMigrations := func() bool {
		v := strings.ToLower(strings.TrimSpace(os.Getenv("CL_SKIP_MIGRATIONS")))
		return v == "1" || v == "true"
	}()
	if skipMigrations && !migrateOnly {
		slog.Info("startup migrations skipped (CL_SKIP_MIGRATIONS set); the migrate Job owns schema changes")
	} else {
		release, err := dia.AcquireMigrationLock()
		if err != nil {
			return fmt.Errorf("acquire migration lock: %w", err)
		}
		if err := dia.RunMigrations(context.Background()); err != nil {
			release()
			return fmt.Errorf("run migrations: %w", err)
		}
		release()
	}
	if migrateOnly {
		slog.Info("migrate-only: schema is up to date, exiting", "driver", cfg.Database.Driver)
		return nil
	}

	// Redis is optional for the guard: Community engines run without it; a
	// configured-but-down instance only logs (InitRedis degrades at startup
	// and retries at runtime).
	rdb := app.InitRedis(&cfg.Redis)

	cp, err := crypto.NewProvider(cfg.Crypto.Mode)
	if err != nil {
		return fmt.Errorf("create crypto provider: %w", err)
	}
	// 授权门：商业构建经 Hooks 注入 license.Init（激活/心跳），Community
	// 构建缺省 NoopGate（全档通过）。
	licenseInit := hooks.LicenseInit
	if licenseInit == nil {
		licenseInit = license.CommunityInit
	}
	gate := licenseInit(cfg.License, db, cp)
	defer gate.Stop()

	tokenRepo := repository.NewGuardTokenRepo(db)
	ingestCache := newTokenCache()
	traceStore := NewTraceStore(time.Duration(cfg.Guard.TraceStore.TTLMinutes)*time.Minute, cfg.Guard.TraceStore.MaxEntries)
	traceRecorder := NewTraceRecorder(repository.NewGuardTraceRepo(db), traceStore)
	detectorPool := worker.NewPool("guard", cfg.Guard.Queue.Workers, cfg.Guard.Queue.QueueSize, cfg.Guard.Queue.OverflowCap)
	grSvc := guardrail.NewGuardrailService(db, rdb)
	a := &App{
		Cfg:       cfg,
		DB:        db,
		Dia:       dia,
		Rdb:       rdb,
		Gate:      gate,
		Settings:  settings.NewProvider(db, cfg, nil),
		Guardrail: grSvc,
		CP:        cp,

		TokenRepo:   tokenRepo,
		IngestCache: ingestCache,
		Tokens:      NewIngestTokenService(tokenRepo, ingestCache),
		Traces:      traceRecorder,
		Pool:        detectorPool,
		Pipeline:    NewPipeline(grSvc, repository.NewGuardEventRepo(db), traceRecorder, detectorPool, time.Duration(cfg.Guard.DetectBudgetMs)*time.Millisecond),

		Idem:    newIdemStore(15*time.Minute, 100000),
		Limiter: NewIngestRateLimiter(rdb, cfg.Guard.Ingest.TokenRPM, cfg.Guard.Ingest.OrgRPM),
	}
	// 流式分片缓冲：空闲终检走 SubmitAsync（fire-and-forget，与镜像终检
	// 同语义）；治理参数与配对窗口共用 trace_store 配置段。
	a.Streams = NewStreamStore(time.Duration(cfg.Guard.TraceStore.TTLMinutes)*time.Minute, cfg.Guard.TraceStore.MaxEntries,
		func(req *DetectRequest) { a.Pipeline.SubmitAsync(req) })
	a.Guardrail.RefreshSettings(context.Background())

	// 可观测面：/metrics（与网关同款认证——gateway.auth_key 为空=放行，
	// 与网关默认部署姿态一致；生产应配置）+ 检测池深度 gauge + 每日留存
	// 清理（retention_days<=0 关闭）。清理随进程退出取消，不进优雅停机
	// 预算（无在途状态，下一轮补删）。
	registerQueueDepthGauge(detectorPool)
	rctx, rcancel := context.WithCancel(context.Background())
	defer rcancel()
	go NewRetentionJob(repository.NewGuardEventRepo(db), repository.NewGuardTraceRepo(db), cfg.Guard.RetentionDays).RunLoop(rctx)

	// JWT secret 强度（Step 4 推迟项，随 Admin API 接入补齐）：与网关
	// buildAuth 同规则——默认值/<32 字符拒绝启动。
	if cfg.Admin.IsDefaultJWTSecret() || cfg.Admin.IsJWTSecretInsecure() {
		return fmt.Errorf("insecure admin JWT secret (length %d): set admin.jwt_secret to at least 32 random characters", len(cfg.Admin.JWTSecret))
	}

	// 首启动 seed：系统角色（sqlite/mysql 压缩基线不含角色种子）、默认
	// org（初始租户）、平台管理员。
	if err := Bootstrap(db, &cfg.Admin); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	// RBAC 缓存在 Bootstrap 之后装载（管理员动作位由 seed 写入）；M1 角色
	// 动作位只随版本变化，启动时装载一次即可，网关的周期刷新留给 M2。
	permCache := middleware.NewPermissionCache(repository.NewRoleRepo(db))
	if err := permCache.Load(); err != nil {
		return fmt.Errorf("load permission cache: %w", err)
	}
	a.PermCache = permCache

	// 商业接线（overlay）：引擎种子与事件 handler（告警/incident）。
	// Community 构建不设置 PostSetup，此处零执行。
	if hooks.PostSetup != nil {
		hooks.PostSetup(a)
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:           a.newRouter(),
		ReadTimeout:       cfg.Server.ReadTimeout,
		ReadHeaderTimeout: readHeaderTimeout(cfg.Server.ReadTimeout),
		IdleTimeout:       90 * time.Second,
		// The guard M1 API is plain request/response JSON — no SSE — so a
		// global write timeout is safe here (unlike the gateway).
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	slog.Info("crosslink-guard listening",
		"addr", srv.Addr, "version", version.Version, "tier", gate.CurrentTier())
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		_ = dia.Shutdown(db)
		return fmt.Errorf("serve: %w", err)
	case <-quit:
	}

	slog.Info("shutting down crosslink-guard...")
	shutdownBudget := cfg.Server.ShutdownTimeout
	if shutdownBudget <= 0 {
		shutdownBudget = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("guard forced to shutdown", "error", err)
	}
	// 排空检测池：HTTP 已停止接收，池内任务只剩已入队的检测与落库。
	pctx, pcancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer pcancel()
	a.Pool.Shutdown(pctx)
	if err := dia.Shutdown(db); err != nil {
		slog.Error("database shutdown error", "error", err)
	}
	slog.Info("crosslink-guard exited")
	return nil
}

// newRouter builds the M1 router; later steps attach the ingest and admin
// groups here.
func (a *App) newRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	if len(a.Cfg.Server.TrustedProxies) > 0 {
		r.SetTrustedProxies(a.Cfg.Server.TrustedProxies)
		slog.Warn("TrustedProxies configured — ensure only known reverse proxy IPs are listed to prevent X-Forwarded-For spoofing", "proxies", a.Cfg.Server.TrustedProxies)
	} else {
		r.SetTrustedProxies(nil)
	}
	r.GET("/healthz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if sqlDB, err := a.DB.DB(); err == nil {
			if err := sqlDB.PingContext(ctx); err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unhealthy", "database": err.Error()})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "version": version.Version})
	})
	// /metrics：prometheus 默认注册表（guard 指标 + worker 池指标 + Go 运行
	// 时）。认证复用网关 GatewayMetricsAuth（gateway.auth_key，Bearer 或
	// x-api-key；为空=放行——内网部署默认姿态，公网暴露必须配置）。
	r.GET("/metrics", middleware.GatewayMetricsAuth(a.Cfg.Gateway.AuthKey), gin.WrapH(promhttp.Handler()))
	a.registerIngestRoutes(r)
	a.registerMirrorRoutes(r)
	a.registerAdminRoutes(r)
	registerConsoleRoutes(r)
	if hooks.RegisterRoutes != nil {
		hooks.RegisterRoutes(a, r)
	}
	return r
}

// readHeaderTimeout caps header-reading time at 15s like the gateway
// (slowloris header holding must not occupy a conn for the full ReadTimeout).
func readHeaderTimeout(read time.Duration) time.Duration {
	if read > 0 && read < 15*time.Second {
		return read
	}
	return 15 * time.Second
}
