package config

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Server         ServerConfig         `mapstructure:"server"`
	Database       DatabaseConfig       `mapstructure:"database"`
	Redis          RedisConfig          `mapstructure:"redis"`
	Gateway        GatewayConfig        `mapstructure:"gateway"`
	Admin          AdminConfig          `mapstructure:"admin"`
	RateLimit      RateLimitConfig      `mapstructure:"rate_limit"`
	Logging        LoggingConfig        `mapstructure:"logging"`
	Cache          CacheConfig          `mapstructure:"cache"`
	SecretManager  SecretManagerConfig  `mapstructure:"secret_manager"`
	SMTP           SMTPConfig           `mapstructure:"smtp"`
	GuardrailAlert GuardrailAlertConfig `mapstructure:"guardrail_alert"`
	IPBinding      IPBindingConfig      `mapstructure:"ip_binding"`
	CORS           CORSConfig           `mapstructure:"cors"`
	License        LicenseConfig        `mapstructure:"license"`
	MCP            MCPConfig            `mapstructure:"mcp"`
	Crypto         CryptoConfig         `mapstructure:"crypto"`
	DataLens       DataLensConfig       `mapstructure:"datalens"`
	Captcha        CaptchaConfig        `mapstructure:"captcha"`
	Demo           DemoConfig           `mapstructure:"demo"`
	Usage          UsageConfig          `mapstructure:"usage"`
}

// UsageConfig sizes the async usage-logging worker pool. Tasks carry billing
// data: when the queue, the overflow allowance AND the disk spill are all
// exhausted, tasks are dropped (counted by cl_worker_tasks_dropped_total).
type UsageConfig struct {
	Workers     int `mapstructure:"workers"`      // concurrent DB writers
	QueueSize   int `mapstructure:"queue_size"`   // buffered queue depth
	OverflowCap int `mapstructure:"overflow_cap"` // extra detached tasks allowed when the queue is full
	// SpoolDir arms the disk spill fallback for billing rows the pool cannot
	// accept (outage/degradation): rows persist as JSONL and a replay loop
	// feeds them back once the pool recovers. Empty disables spooling.
	SpoolDir string `mapstructure:"spool_dir"`
	// SpoolMaxBytes caps the spill file; beyond it rows drop (counted).
	SpoolMaxBytes int64 `mapstructure:"spool_max_bytes"`
	// SpoolEmergencyMaxBytes caps the second-tier overflow WAL that takes
	// rows at the DB+WAL double-failure point (main WAL at cap/unavailable
	// AND the synchronous DB commit failed). While it holds a row the row is
	// durable-pending and replay drains it after DB recovery; a real loss
	// then requires the disk itself to fail. 0/negative disables the tier
	// (legacy r34 behavior: counted loss + integrity block). Env:
	// CL_USAGE_SPOOL_EMERGENCY_MAX_BYTES.
	SpoolEmergencyMaxBytes int64 `mapstructure:"spool_emergency_max_bytes"`
	// LossPolicy decides what happens when a billing row can be persisted
	// NOWHERE (DB write failed AND the WAL could not take it):
	//   count (legacy) — increment cl_usage_rows_lost_total, keep serving;
	//   block (DECIDED DEFAULT, 2026-10-19) — ledger-integrity-first: engage
	//     the billing-integrity block, reject NEW billable requests with 503
	//     until durability recovers. Env: CL_USAGE_LOSS_POLICY.
	LossPolicy string `mapstructure:"loss_policy"`
	// IntegrityStateFile persists an engaged integrity block across process
	// restarts (r34: a restart must not silently clear an unfixed block).
	// Empty disables persistence (in-process block only). Env:
	// CL_USAGE_INTEGRITY_STATE_FILE.
	IntegrityStateFile string `mapstructure:"integrity_state_file"`
}

type ServerConfig struct {
	Port           int           `mapstructure:"port"`
	ReadTimeout    time.Duration `mapstructure:"read_timeout"`
	WriteTimeout   time.Duration `mapstructure:"write_timeout"`
	TrustedProxies []string      `mapstructure:"trusted_proxies"`
	// StreamMaxDuration caps the total duration of one SSE stream to the
	// upstream provider (0 = uncapped). Client disconnects always propagate
	// regardless of this value; the cap only bounds runaway streams whose
	// client stays connected.
	StreamMaxDuration time.Duration `mapstructure:"stream_max_duration"`
	// StreamIdleTimeout closes an established SSE stream when the upstream
	// sends no bytes for this long (0 = disabled). A connected-but-silent
	// upstream otherwise pins one goroutine + one upstream connection per
	// request indefinitely. Mid-stream heartbeats (e.g. `: ping`) reset it.
	StreamIdleTimeout time.Duration `mapstructure:"stream_idle_timeout"`
	// StreamWriteTimeout arms a deadline before EVERY response Write/Flush on
	// the real connection (0 = disabled). http.Server.WriteTimeout is
	// intentionally 0 (it would kill healthy long SSE streams at a fixed
	// total age); instead each individual write must complete within this
	// budget. A consumer that keeps the connection open but never reads
	// therefore surfaces as a write timeout and unwinds the handler,
	// admission slot, upstream stream and billing resources — a
	// request-context deadline cannot unblock a parked TCP write. Slow but
	// live readers are unaffected (each write completes as the socket
	// drains).
	StreamWriteTimeout time.Duration `mapstructure:"stream_write_timeout"`
	// Shutdown phase budgets. Defaults preserve the historical behavior
	// (60s stream drain + 10s http.Server shutdown + 10s usage flush). Their
	// SUM plus DB/Redis teardown bounds the container stop grace period — a
	// grace shorter than drain+shutdown+flush truncates in-flight streams and
	// loses buffered usage rows.
	DrainTimeout       time.Duration `mapstructure:"drain_timeout"`
	ShutdownTimeout    time.Duration `mapstructure:"shutdown_timeout"`
	WorkerFlushTimeout time.Duration `mapstructure:"worker_flush_timeout"`
}

type DatabaseConfig struct {
	Driver     string `mapstructure:"driver"`
	Host       string `mapstructure:"host"`
	Port       int    `mapstructure:"port"`
	User       string `mapstructure:"user"`
	Password   string `mapstructure:"password"`
	DBName     string `mapstructure:"dbname"`
	SSLMode    string `mapstructure:"sslmode"`
	SQLitePath string `mapstructure:"sqlite_path"`
	// Timezone pins the session timezone used for SQL date bucketing
	// (DATE(created_at)) and the day boundaries of usage statistics. Empty =
	// server process local time. IANA name, e.g. "Asia/Shanghai".
	Timezone string `mapstructure:"timezone"`
	// Pool overrides. Zero = dialect built-in defaults (PG/MySQL: 100 open /
	// 50 idle / 5m lifetime / 1m idle-time; SQLite is single-connection by
	// design). Set these when running multiple replicas against a shared
	// postgres: total steady connections = replicas x MaxOpenConns, which
	// must fit the server's max_connections with headroom for admin tooling,
	// migrations and surge capacity during rolling updates.
	MaxOpenConns    int           `mapstructure:"max_open_conns"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
	ConnMaxIdleTime time.Duration `mapstructure:"conn_max_idle_time"`
}

func (d DatabaseConfig) DSN() string {
	switch d.Driver {
	case "postgres", "kingbase", "kingbasees":
		return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
			d.Host, d.Port, d.User, d.Password, d.DBName, d.SSLMode)
	case "mysql", "oceanbase":
		return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=true&loc=UTC",
			d.User, d.Password, d.Host, d.Port, d.DBName)
	case "sqlite":
		return d.SQLitePath
	default:
		return ""
	}
}

func (d DatabaseConfig) DSNURL() string {
	switch d.Driver {
	case "postgres", "kingbase", "kingbasees":
		u := &url.URL{
			Scheme:   "postgres",
			User:     url.UserPassword(d.User, d.Password),
			Host:     fmt.Sprintf("%s:%d", d.Host, d.Port),
			Path:     d.DBName,
			RawQuery: "sslmode=" + d.SSLMode,
		}
		return u.String()
	case "mysql", "oceanbase":
		return fmt.Sprintf("mysql://%s@tcp(%s:%d)/%s",
			url.UserPassword(d.User, d.Password).String(), d.Host, d.Port, d.DBName)
	default:
		return ""
	}
}

type RedisConfig struct {
	Host         string `mapstructure:"host"`
	Port         int    `mapstructure:"port"`
	Password     string `mapstructure:"password"`
	DB           int    `mapstructure:"db"`
	PoolSize     int    `mapstructure:"pool_size"`
	MinIdleConns int    `mapstructure:"min_idle_conns"`
}

func (r RedisConfig) Addr() string {
	return fmt.Sprintf("%s:%d", r.Host, r.Port)
}

type GatewayConfig struct {
	AuthKey string `mapstructure:"auth_key"`
	BaseURL string `mapstructure:"base_url"`
	// InternalAllowCIDRs permits outbound provider/MCP/video connections to
	// these restricted CIDRs (e.g. an on-prem VPC range) so self-hosted LLM
	// providers on internal addresses work. Empty (default) = block all
	// restricted ranges; cloud-metadata (169.254.x.x) and loopback stay blocked
	// unless explicitly listed. Env override: CL_GATEWAY_INTERNAL_ALLOW_CIDRS.
	InternalAllowCIDRs []string `mapstructure:"internal_allow_cidrs"`
	// InternalAllowCIDRsFromEnv reports whether the static value above came
	// from the env var (true) or the YAML file (false). Not config-bound:
	// set during Load; consumed by the B5 allowlist service to label the
	// active source ("env" vs "yaml") and compute the reset fallback.
	InternalAllowCIDRsFromEnv bool `mapstructure:"-"`
	// ConcurrencyLimit caps the number of in-flight gateway (LLM proxy)
	// requests. 0 means use the built-in default (2000). Lower it for
	// resource-constrained or public demo deployments. Env override:
	// CL_GATEWAY_CONCURRENCY_LIMIT.
	ConcurrencyLimit int `mapstructure:"concurrency_limit"`
	// ProviderTimeout bounds ONE non-streaming upstream attempt (per provider,
	// before fallback moves on). Long reasoning-model generations exceed the
	// old hardcoded 30s and failed as 502s; 0 means the built-in default.
	// Streaming is NOT affected (its caps are stream_max_duration /
	// stream_idle_timeout). Env override: CL_GATEWAY_PROVIDER_TIMEOUT.
	ProviderTimeout time.Duration `mapstructure:"provider_timeout"`
	// UpstreamMaxConnsPerHost caps concurrent upstream connections per host
	// per provider instance (0 = default 200). Concurrent streams beyond the
	// cap queue on the pool and inflate first-token latency; raise for
	// high-concurrency deployments or add more provider rows for the same
	// upstream (pools are per provider instance).
	UpstreamMaxConnsPerHost int `mapstructure:"upstream_max_conns_per_host"`
	// UpstreamMaxIdleConnsPerHost / UpstreamMaxIdleConns size the upstream
	// idle pool (0 = defaults 20 per host / 400 total). Released conns beyond
	// the idle cap are closed and redialed: at high stream-completion rates
	// the defaults churned ~111 closes/s (r12c/r13) — raise toward the steady
	// concurrent-stream count per upstream host. Env overrides:
	// CL_GATEWAY_UPSTREAM_MAX_IDLE_CONNS_PER_HOST / _MAX_IDLE_CONNS.
	UpstreamMaxIdleConnsPerHost int `mapstructure:"upstream_max_idle_conns_per_host"`
	UpstreamMaxIdleConns        int `mapstructure:"upstream_max_idle_conns"`
	// BudgetMarkerTTL bounds the lifetime of the F1′ per-request
	// reservation/settlement idempotency markers (resvmark/settledmark) and
	// the orphan-reaper latency: an unsettled reservation is removed once
	// its marker expires, preventing permanent over-count from crashed
	// requests. Must exceed stream_max_duration plus any replay window.
	// Env override: CL_GATEWAY_BUDGET_MARKER_TTL.
	BudgetMarkerTTL string `mapstructure:"budget_marker_ttl"`
	// BudgetReapInterval bounds how long an EXPIRED reservation may hold
	// admission headroom after its marker TTL passed: the independent reap
	// loop runs on this cadence (in addition to the hourly calibration
	// round's embedded reap). Worst-case recovery bound =
	// markerTTL + reapInterval. The loop only removes ALREADY-EXPIRED
	// markers (exactly-once across replicas), so shrinking the interval
	// never releases a live/long-stream/cross-period request — only the
	// TTL does, and it must stay >= max request lifetime + replay window.
	// Env override: CL_GATEWAY_BUDGET_REAP_INTERVAL; "0" or negative
	// disables the independent loop (legacy hourly-only reap).
	BudgetReapInterval string `mapstructure:"budget_reap_interval"`
	// BudgetMetadataStaleWindow bounds how long a last-known-good team/org
	// budget snapshot may enforce after its TTL while the metadata DB is
	// unreachable (fail-safe enforcement instead of the r61 key-only
	// bypass). Limit changes propagate within TTL+staleWindow; a COLD cache
	// during an outage rejects the request (fail-closed). Env:
	// CL_GATEWAY_BUDGET_METADATA_STALE_WINDOW.
	BudgetMetadataStaleWindow string `mapstructure:"budget_metadata_stale_window"`
	// BudgetMetadataTTL overrides the team/org metadata fresh-entry TTL
	// (default 30s, cache-side). Test rounds shorten it to move the
	// fail-safe horizon (write+2*ttl+staleWindow) ahead of other outage
	// timers (auth Redis cache = 60s) and observe the budget branch live;
	// production keeps the default. Env: CL_GATEWAY_BUDGET_METADATA_TTL.
	BudgetMetadataTTL string `mapstructure:"budget_metadata_ttl"`
	// BudgetMetadataLookupTimeout bounds the per-lookup DB round trip inside
	// team/org GetSafe (r61/r78 availability fix). On a cache miss against a
	// down/unreachable DB the request previously held its admission slot for
	// the full driver dial timeout; with this bound the SAME fallback chain
	// (fresh / stale-LKG enforcement / typed fail-closed 503) is reached in
	// bounded time. This is a latency bound only — it never widens admission
	// or bypasses budget/billing. Default 2s. Env:
	// CL_GATEWAY_BUDGET_METADATA_LOOKUP_TIMEOUT.
	BudgetMetadataLookupTimeout string `mapstructure:"budget_metadata_lookup_timeout"`
	// ResolveLookupTimeout bounds the per-lookup DB round trip inside the
	// router's resolveUncached (model-mapping query). Same rationale as the
	// budget knob: a bounded miss reaches the existing degraded negative
	// cache / typed 503 path fast instead of holding a slot for the dial
	// timeout. Default 2s. Env: CL_GATEWAY_RESOLVE_LOOKUP_TIMEOUT.
	ResolveLookupTimeout string `mapstructure:"resolve_lookup_timeout"`
	// AuthLookupTimeout bounds the cold-key DB round trips in
	// KeyService.Validate (hash lookup + once-daily last_used_at write).
	// r78e measured the unbounded form: a blackhole DB (accepts, never
	// answers) held the request until the client hung up (15s curl cap).
	// The bound surfaces as ErrAuthUnavailable -> typed 503 fail-closed;
	// latency only, never widens admission. Default 2s. Env:
	// CL_GATEWAY_AUTH_LOOKUP_TIMEOUT.
	AuthLookupTimeout string `mapstructure:"auth_lookup_timeout"`
	// ModelConfigLookupTimeout bounds the semantic-cache middleware's
	// provider_models config lookup (per-model cache TTL/disabled flags),
	// which runs BEFORE the router's bounded resolver on the non-stream
	// path. On bound the middleware keeps the default cache policy and the
	// bounded resolver still fail-closes admission. Default 2s. Env:
	// CL_GATEWAY_MODEL_CONFIG_LOOKUP_TIMEOUT.
	ModelConfigLookupTimeout string `mapstructure:"model_config_lookup_timeout"`
	// CircuitProbePath is where the circuit prober looks for a models list
	// ("/models" OpenAI-style). A 2xx there closes an open circuit directly;
	// any other outcome falls back to a 1-token real chat probe. Empty string
	// disables the models probe entirely (chat-only probing). Env override:
	// CL_GATEWAY_CIRCUIT_PROBE_PATH.
	CircuitProbePath string `mapstructure:"circuit_probe_path"`
	// InflightBodyBudgetBytes caps the TOTAL bytes of buffered request bodies
	// across all in-flight gateway requests (0 = disabled). The concurrency
	// limit alone bounds request COUNT, not memory: 2000 slots x the 10MiB
	// body ceiling is ~19.5GiB of BodyKey buffers alone. Requests are
	// rejected (503, before buffering — known length via pre-check, unknown
	// length via per-block reservation before each allocation) once the
	// budget is exhausted.
	InflightBodyBudgetBytes int64 `mapstructure:"inflight_body_budget_bytes"`
	// InflightMultipartReserveBytes is the conservative budget reservation
	// for a multipart upload with unknown Content-Length: its handler buffers
	// the form in-process (FormFile → ParseMultipartForm, ~32MB in memory
	// plus file-part copies), so uploads participate in the body budget
	// rather than bypassing it. A known Content-Length below this cap
	// reserves the actual smaller value.
	InflightMultipartReserveBytes int64 `mapstructure:"inflight_multipart_reserve_bytes"`
}

// DemoConfig controls the optional zero-cost demo mode (public /demo try page
// backed by a mock provider + embedded demo key). Disabled by default.
type DemoConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	APIKey  string `mapstructure:"api_key"` // mock-only, embedded in the /demo page
}

type AdminConfig struct {
	Username     string `mapstructure:"username"`
	Password     string `mapstructure:"password"`
	PasswordHash string `mapstructure:"-"`
	JWTSecret    string `mapstructure:"jwt_secret"`
	TokenExpiry  int    `mapstructure:"token_expiry"`
}

// CaptchaConfig configures the login CAPTCHA. The default self-hosted "slider"
// provider works in any environment; cloud providers (overlay) are selectable
// via Provider for deployments with outbound network.
type CaptchaConfig struct {
	Enabled       bool                `mapstructure:"enabled"`
	Provider      string              `mapstructure:"provider"`        // slider | turnstile | tencent | aliyun
	TrustDays     int                 `mapstructure:"trust_days"`      // device-cookie lifetime, 0 = require every login
	TrustIPMask   int                 `mapstructure:"trust_ip_mask"`   // CIDR bits bound into the trust cookie, 0 = none
	RedisFailOpen bool                `mapstructure:"redis_fail_open"` // on Redis failure, allow login (rate-limit backstop)
	Slider        CaptchaSliderConfig `mapstructure:"slider"`
}

type CaptchaSliderConfig struct {
	TolerancePx float64 `mapstructure:"tolerance_px"`
	MinPoints   int     `mapstructure:"min_points"`
	BGWidth     int     `mapstructure:"bg_width"`
	BGHeight    int     `mapstructure:"bg_height"`
	PieceSize   int     `mapstructure:"piece_size"`
}

type RateLimitConfig struct {
	RPM         int  `mapstructure:"rpm"`
	TPM         int  `mapstructure:"tpm"`
	Reservation int  `mapstructure:"tpm_reservation"`
	FailClosed  bool `mapstructure:"fail_closed"`
}

type LoggingConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
}

type CacheConfig struct {
	Enabled       bool          `mapstructure:"enabled"`
	DefaultTTL    time.Duration `mapstructure:"default_ttl"`
	EmbeddingsTTL time.Duration `mapstructure:"embeddings_ttl"`
	MaxBodySize   int           `mapstructure:"max_body_size"`
	// Redis optionally points the response cache at a SEPARATE Redis instance
	// from the quota/budget/idempotency one. Deployments should isolate them:
	// the cache tolerates eviction (allkeys-lru, cheap RAM), while
	// quota/budget/idempotency keys must NOT be evicted (noeviction). Sharing
	// one instance forces one eviction policy over both — either the cache
	// never evicts (RAM blowup) or limits fail open. A different DB number on
	// the same instance does NOT isolate maxmemory/eviction. nil/absent =
	// share the main redis (current behavior).
	Redis *RedisConfig `mapstructure:"redis"`
}

type SecretManagerConfig struct {
	EncryptionKey string        `mapstructure:"encryption_key"`
	CacheTTL      time.Duration `mapstructure:"cache_ttl"`
}

type SMTPConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	From     string `mapstructure:"from"`
}

type GuardrailAlertConfig struct {
	Enabled           bool `mapstructure:"enabled"`
	Concurrency       int  `mapstructure:"concurrency"`
	ContentPreview    bool `mapstructure:"content_preview"`
	ContentPreviewLen int  `mapstructure:"content_preview_len"`
}

type IPBindingConfig struct {
	NotifyCooldownSeconds int `mapstructure:"notify_cooldown_seconds"` // Pro/Enterprise; 0 = default 300
}

type CORSConfig struct {
	AllowedOrigins []string `mapstructure:"allowed_origins"`
}

type LicenseConfig struct {
	ServerURL         string        `mapstructure:"server_url" yaml:"server_url"`
	LicenseKey        string        `mapstructure:"license_key" yaml:"license_key"`
	LicenseFilePath   string        `mapstructure:"license_file" yaml:"license_file"`
	OfflineFilePath   string        `mapstructure:"offline_file" yaml:"offline_file"`
	HeartbeatEnabled  bool          `mapstructure:"heartbeat_enabled" yaml:"heartbeat_enabled"`
	HeartbeatInterval time.Duration `mapstructure:"heartbeat_interval" yaml:"heartbeat_interval"`
	NodeID            string        `mapstructure:"node_id" yaml:"node_id"`
	// U-37 Ed25519 signature verification (user-authorized 2026-10-19;
	// production signing keys held by the user — this tree ships none).
	// VerifyMode off|warn|enforce, DEFAULT off (the enforcement date is the
	// user's call; do not flip unilaterally). VerifyKeys is inline key
	// material or "@<path>": JSON {kid: base64 raw key} or PEM list with
	// optional "# kid=<id>" lines. Env: CL_LICENSE_VERIFY_MODE / _ISSUER /
	// _AUDIENCE / _KEYS.
	VerifyMode     string `mapstructure:"verify_mode" yaml:"verify_mode"`
	VerifyIssuer   string `mapstructure:"verify_issuer" yaml:"verify_issuer"`
	VerifyAudience string `mapstructure:"verify_audience" yaml:"verify_audience"`
	VerifyKeys     string `mapstructure:"verify_keys" yaml:"verify_keys"`
}

type MCPConfig struct {
	Enabled             bool          `mapstructure:"enabled"`
	MaxServers          int           `mapstructure:"max_servers"`
	HealthCheckInterval time.Duration `mapstructure:"health_check_interval"`
	ToolCacheTTL        time.Duration `mapstructure:"tool_cache_ttl"`
	RequestTimeout      time.Duration `mapstructure:"request_timeout"`
	HTTPMaxIdleConns    int           `mapstructure:"http_max_idle_conns"`
	RateLimitEnabled    bool          `mapstructure:"rate_limit_enabled"`
	RateLimitDefaultRPM int           `mapstructure:"rate_limit_default_rpm"`
	LogRetentionDays    int           `mapstructure:"log_retention_days"`
	// AllowStdio enables the stdio MCP transport, which spawns LOCAL subprocesses
	// configured by MCP server admins. Disabled by default because stdio lets an
	// admin with mcp:create execute arbitrary commands on the gateway host — a
	// cross-tenant RCE risk in multi-tenant deployments. The platform owner must
	// explicitly opt in. HTTP/SSE transports are unaffected. (Commercial build.)
	AllowStdio bool `mapstructure:"allow_stdio"`
}

type CryptoConfig struct {
	Mode string `mapstructure:"mode"` // "standard" (default) or "gm"
}

type DataLensConfig struct {
	Enabled   bool              `mapstructure:"enabled"`
	Retention DataLensRetention `mapstructure:"retention"`
	Agg       DataLensAggConfig `mapstructure:"aggregation"`
	FromName  string            `mapstructure:"from_name"`
	FromAddr  string            `mapstructure:"from_address"`
}

type DataLensRetention struct {
	RawLogsDays int `mapstructure:"raw_logs_days"`
	HourlyDays  int `mapstructure:"hourly_metrics_days"`
	DailyDays   int `mapstructure:"daily_metrics_days"`
}

type DataLensAggConfig struct {
	Interval            string   `mapstructure:"interval"`
	Lookback            string   `mapstructure:"lookback"`
	BackfillDays        int      `mapstructure:"backfill_days"`
	BackfillConcurrency int      `mapstructure:"backfill_concurrency"`
	Levels              []string `mapstructure:"levels"`
}

// Validate checks that config values are within acceptable ranges.
// Returns an error describing the first invalid field found.
func (c *Config) Validate() error {
	validSSLModes := map[string]bool{"disable": true, "require": true, "verify-ca": true, "verify-full": true, "allow": true, "prefer": true}
	validLogLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	validLogFormats := map[string]bool{"json": true, "text": true}

	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535, got %d", c.Server.Port)
	}
	switch c.Database.Driver {
	case "", "postgres":
		c.Database.Driver = "postgres"
		if c.Database.Port == 0 {
			c.Database.Port = 5432
		}
		if c.Database.Port < 1 || c.Database.Port > 65535 {
			return fmt.Errorf("database.port must be between 1 and 65535, got %d", c.Database.Port)
		}
		if c.Database.Host == "" {
			return fmt.Errorf("database.host is required for postgres")
		}
		if c.Database.DBName == "" {
			return fmt.Errorf("database.dbname is required for postgres")
		}
		if c.Database.SSLMode != "" && !validSSLModes[c.Database.SSLMode] {
			return fmt.Errorf("database.sslmode invalid: %q", c.Database.SSLMode)
		}
	case "kingbase", "kingbasees":
		c.Database.Driver = "kingbasees"
		if c.Database.Port == 0 {
			c.Database.Port = 54321
		}
		if c.Database.Port < 1 || c.Database.Port > 65535 {
			return fmt.Errorf("database.port must be between 1 and 65535, got %d", c.Database.Port)
		}
		if c.Database.Host == "" {
			return fmt.Errorf("database.host is required for kingbasees")
		}
		if c.Database.DBName == "" {
			return fmt.Errorf("database.dbname is required for kingbasees")
		}
		if c.Database.SSLMode != "" && !validSSLModes[c.Database.SSLMode] {
			return fmt.Errorf("database.sslmode invalid: %q", c.Database.SSLMode)
		}
	case "mysql":
		if c.Database.Port == 0 {
			c.Database.Port = 3306
		}
		if c.Database.Port < 1 || c.Database.Port > 65535 {
			return fmt.Errorf("database.port must be between 1 and 65535, got %d", c.Database.Port)
		}
		if c.Database.Host == "" {
			return fmt.Errorf("database.host is required for mysql")
		}
		if c.Database.DBName == "" {
			return fmt.Errorf("database.dbname is required for mysql")
		}
	case "oceanbase":
		if c.Database.Port == 0 {
			c.Database.Port = 2883
		}
		if c.Database.Port < 1 || c.Database.Port > 65535 {
			return fmt.Errorf("database.port must be between 1 and 65535, got %d", c.Database.Port)
		}
		if c.Database.Host == "" {
			return fmt.Errorf("database.host is required for oceanbase")
		}
		if c.Database.DBName == "" {
			return fmt.Errorf("database.dbname is required for oceanbase")
		}
	case "sqlite":
		if c.Database.SQLitePath == "" {
			return fmt.Errorf("database.sqlite_path is required for sqlite")
		}
	default:
		return fmt.Errorf("database.driver must be postgres, kingbasees, mysql, oceanbase, or sqlite, got %q", c.Database.Driver)
	}
	if c.Redis.Port < 1 || c.Redis.Port > 65535 {
		return fmt.Errorf("redis.port must be between 1 and 65535, got %d", c.Redis.Port)
	}
	if c.Redis.DB < 0 || c.Redis.DB > 15 {
		return fmt.Errorf("redis.db must be between 0 and 15, got %d", c.Redis.DB)
	}
	if c.RateLimit.RPM < 0 {
		return fmt.Errorf("rate_limit.rpm must be >= 0, got %d", c.RateLimit.RPM)
	}
	if c.RateLimit.TPM < 0 {
		return fmt.Errorf("rate_limit.tpm must be >= 0, got %d", c.RateLimit.TPM)
	}
	if c.Cache.DefaultTTL < 0 {
		return fmt.Errorf("cache.default_ttl must be >= 0, got %s", c.Cache.DefaultTTL)
	}
	if c.Admin.TokenExpiry <= 0 {
		return fmt.Errorf("admin.token_expiry must be > 0, got %d", c.Admin.TokenExpiry)
	}
	if c.Logging.Level != "" && !validLogLevels[c.Logging.Level] {
		return fmt.Errorf("logging.level invalid: %q", c.Logging.Level)
	}
	if c.Logging.Format != "" && !validLogFormats[c.Logging.Format] {
		return fmt.Errorf("logging.format invalid: %q", c.Logging.Format)
	}
	if c.Crypto.Mode != "" && c.Crypto.Mode != "standard" && c.Crypto.Mode != "gm" {
		return fmt.Errorf("crypto.mode must be 'standard' or 'gm', got: %s", c.Crypto.Mode)
	}
	if c.Crypto.Mode == "gm" && c.SecretManager.EncryptionKey != "" {
		encKey, err := base64.StdEncoding.DecodeString(c.SecretManager.EncryptionKey)
		if err != nil {
			return fmt.Errorf("GM mode: invalid encryption key (not base64): %w", err)
		}
		if len(encKey) != 16 {
			return fmt.Errorf("GM mode requires 16-byte encryption key (SM4), got %d bytes", len(encKey))
		}
	}
	if c.Demo.Enabled && strings.TrimSpace(c.Demo.APIKey) == "" {
		return fmt.Errorf("demo.enabled is true but demo.api_key is empty")
	}
	return nil
}

func (a AdminConfig) IsDefaultJWTSecret() bool {
	return a.JWTSecret == "" || a.JWTSecret == "change-me-to-a-random-secret" || a.JWTSecret == "dev-secret-change-in-production"
}

func (a AdminConfig) IsDefaultPassword() bool {
	return a.Password == "changeme"
}

func (a AdminConfig) IsJWTSecretInsecure() bool {
	return len(a.JWTSecret) < 32
}

// configFilePath stores the resolved config file path from Load() for later writes.
var configFilePath string

func Load(configPath string) (*Config, error) {
	v := viper.New()

	v.SetConfigName("config")
	v.SetConfigType("yaml")
	if configPath != "" {
		v.AddConfigPath(configPath)
	}
	v.AddConfigPath("configs")
	v.AddConfigPath(".")

	v.SetEnvPrefix("CL")
	// Map nested config keys (e.g. "database.driver") to underscore env vars
	// (e.g. "CL_DATABASE_DRIVER"). Without this, Viper looks for a literal-dot
	// env var ("CL_DATABASE.DRIVER") which no shell can set — so every nested
	// CL_* override used by the docker-compose files silently did nothing.
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// A set-but-blank CL_GATEWAY_INTERNAL_ALLOW_CIDRS would otherwise clobber
	// the YAML value at the viper layer (AutomaticEnv applies to keys that
	// exist in the config file too); treat blank as unset for this process.
	if raw := os.Getenv("CL_GATEWAY_INTERNAL_ALLOW_CIDRS"); strings.TrimSpace(raw) == "" {
		_ = os.Unsetenv("CL_GATEWAY_INTERNAL_ALLOW_CIDRS")
	}

	setDefaults(v)

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("read config: %w", err)
		}
		// Config file not found is OK when env vars provide all settings
		slog.Info("no config file found, using env vars and defaults")
	} else {
		configFilePath, _ = filepath.Abs(v.ConfigFileUsed())
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	// gateway.internal_allow_cidrs: viper cannot carry env strings into a
	// []string field, so the explicit env override is handled here. Precedence
	// matches viper's own (env > config file > default): a non-empty
	// CL_GATEWAY_INTERNAL_ALLOW_CIDRS (comma-separated CIDRs) replaces the
	// YAML value; an unset or whitespace-only env leaves the YAML value in
	// place. Entries are validated later by guardrail.SetInternalAllowlist,
	// which applies the whole list atomically or stays strict.
	// InternalAllowCIDRsFromEnv records which static layer supplied the value
	// (B5 reports the provenance and computes the reset fallback with it).
	if raw := strings.TrimSpace(os.Getenv("CL_GATEWAY_INTERNAL_ALLOW_CIDRS")); raw != "" {
		cidrs := make([]string, 0, 4)
		for _, part := range strings.Split(raw, ",") {
			if part = strings.TrimSpace(part); part != "" {
				cidrs = append(cidrs, part)
			}
		}
		cfg.Gateway.InternalAllowCIDRs = cidrs
		cfg.Gateway.InternalAllowCIDRsFromEnv = true
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.read_timeout", "30s")
	// write_timeout is intentionally NOT applied to the http.Server: a global
	// WriteTimeout kills long SSE streams mid-flight. Kept only so existing
	// YAML keeps parsing; a startup warning flags it if set.
	v.SetDefault("server.write_timeout", "0s")
	// Whole-stream and idle caps for SSE (0 = uncapped / disabled). Defaults
	// chosen defensively for production: generous enough for long reasoning
	// streams, finite enough that stalled/abandoned streams cannot pin
	// goroutines and upstream connections forever.
	v.SetDefault("server.stream_max_duration", "30m")
	v.SetDefault("server.stream_idle_timeout", "180s")
	v.SetDefault("server.stream_write_timeout", "180s")
	v.SetDefault("server.drain_timeout", "60s")
	v.SetDefault("server.shutdown_timeout", "10s")
	v.SetDefault("server.worker_flush_timeout", "10s")

	v.SetDefault("usage.workers", 15)
	v.SetDefault("usage.queue_size", 1000)
	v.SetDefault("usage.overflow_cap", 200)
	v.SetDefault("usage.spool_dir", "data/usage-spool")
	v.SetDefault("usage.loss_policy", "block")
	v.SetDefault("usage.integrity_state_file", "data/billing-integrity.state")
	v.SetDefault("usage.spool_max_bytes", 100<<20)
	// Emergency overflow WAL: 1 GiB default covers a long DB outage at high
	// in-flight rates (bounded by the admission concurrency); set 0/negative
	// to disable the tier entirely.
	v.SetDefault("usage.spool_emergency_max_bytes", 1<<30)

	v.SetDefault("database.driver", "postgres")
	v.SetDefault("database.host", "localhost")
	v.SetDefault("database.port", 5432)
	v.SetDefault("database.user", "crosslink")
	v.SetDefault("database.password", "")
	v.SetDefault("database.dbname", "crosslink")
	v.SetDefault("database.sslmode", "disable")
	v.SetDefault("database.sqlite_path", "")
	// Pool keys MUST have defaults or viper silently ignores their env
	// overrides (CL_DATABASE_*) — same trap as inflight_body_budget_bytes.
	// 0 keeps the dialect built-ins (PG/MySQL: 100 open / 50 idle / 5m
	// lifetime / 1m idle-time; SQLite single-connection).
	v.SetDefault("database.max_open_conns", 0)
	v.SetDefault("database.max_idle_conns", 0)
	v.SetDefault("database.conn_max_lifetime", 0)
	v.SetDefault("database.conn_max_idle_time", 0)
	v.SetDefault("database.timezone", "")

	v.SetDefault("redis.host", "localhost")
	v.SetDefault("redis.port", 6379)
	v.SetDefault("redis.password", "")
	v.SetDefault("redis.db", 0)
	v.SetDefault("redis.pool_size", 500)
	v.SetDefault("redis.min_idle_conns", 50)

	v.SetDefault("admin.username", "admin")
	v.SetDefault("admin.password", "changeme")
	v.SetDefault("admin.jwt_secret", "change-me-to-a-random-secret")
	v.SetDefault("admin.token_expiry", 24)

	v.SetDefault("captcha.enabled", false)
	v.SetDefault("captcha.provider", "slider")
	v.SetDefault("captcha.trust_days", 7)
	v.SetDefault("captcha.trust_ip_mask", 24)
	v.SetDefault("captcha.redis_fail_open", true)
	v.SetDefault("captcha.slider.tolerance_px", 5)
	v.SetDefault("captcha.slider.min_points", 5)
	v.SetDefault("captcha.slider.bg_width", 300)
	v.SetDefault("captcha.slider.bg_height", 150)

	v.SetDefault("gateway.concurrency_limit", 2000)
	v.SetDefault("gateway.provider_timeout", "180s")
	v.SetDefault("gateway.upstream_max_conns_per_host", 200)
	// Registered (0 = defaults 20/400) so the CL_GATEWAY_UPSTREAM_MAX_IDLE_*
	// env overrides are honored env-only — viper's AutomaticEnv only resolves
	// keys it already knows.
	v.SetDefault("gateway.upstream_max_idle_conns_per_host", 0)
	v.SetDefault("gateway.upstream_max_idle_conns", 0)
	v.SetDefault("gateway.budget_marker_ttl", "24h")
	// Registered so CL_GATEWAY_BUDGET_METADATA_STALE_WINDOW / _TTL env
	// overrides are honored env-only (viper AutomaticEnv only resolves keys
	// it knows — r70 finding: without this the env is silently inert and the
	// cache-side defaults 10m/30s run instead).
	v.SetDefault("gateway.budget_metadata_stale_window", "10m")
	v.SetDefault("gateway.budget_metadata_ttl", "30s")
	// r78 availability bounds: registered env-only keys, same rationale as
	// the two above (unregistered env is silently inert under AutomaticEnv).
	v.SetDefault("gateway.budget_metadata_lookup_timeout", "2s")
	v.SetDefault("gateway.resolve_lookup_timeout", "2s")
	// r78e follow-up bounds: the two remaining unbounded request-path DB
	// lookups (auth cold-key hash lookup; semantic-cache provider_models
	// config lookup), registered env-only keys like the two above.
	v.SetDefault("gateway.auth_lookup_timeout", "2s")
	v.SetDefault("gateway.model_config_lookup_timeout", "2s")
	// Registered so CL_GATEWAY_BUDGET_REAP_INTERVAL env-only overrides are honored
	// (template gap found by the 2026-10-22 param audit: past rounds set it via
	// yaml only; the env path was silently inert without registration).
	v.SetDefault("gateway.budget_reap_interval", "5m")
	// F-12: circuit-probe models path is a deployment property of the
	// upstream, not an adapter constant. Registered with a default so
	// CL_GATEWAY_CIRCUIT_PROBE_PATH env override is honored; set "" for
	// chat-only probing.
	v.SetDefault("gateway.circuit_probe_path", "/models")
	// Registered (0 = disabled) so CL_GATEWAY_INFLIGHT_BODY_BUDGET_BYTES /
	// CL_GATEWAY_INFLIGHT_MULTIPART_RESERVE_BYTES env overrides are honored
	// env-only — viper's AutomaticEnv only resolves keys it already knows.
	v.SetDefault("gateway.inflight_body_budget_bytes", 0)
	v.SetDefault("gateway.inflight_multipart_reserve_bytes", 32<<20)
	// Empty defaults so CL_GATEWAY_AUTH_KEY / CL_ENCRYPTION_KEY env overrides
	// are honored in docker (where no config.yaml is present, AutomaticEnv only
	// resolves env for keys Viper already knows).
	v.SetDefault("gateway.auth_key", "")
	// NOTE: gateway.internal_allow_cidrs is deliberately NOT registered with
	// viper (no SetDefault/BindEnv): AutomaticEnv would then let a set-but-
	// empty env clobber the YAML value at the viper layer. The env override is
	// handled entirely in Load, which treats unset/whitespace env as "keep the
	// YAML value".
	v.SetDefault("secret_manager.encryption_key", "")
	v.SetDefault("demo.enabled", false)
	v.SetDefault("demo.api_key", "")
	v.SetDefault("captcha.slider.piece_size", 44)

	v.SetDefault("rate_limit.rpm", 0)
	v.SetDefault("rate_limit.tpm", 0)
	v.SetDefault("rate_limit.tpm_reservation", 2000)
	v.SetDefault("rate_limit.fail_closed", false)

	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "json")

	v.SetDefault("cache.enabled", true)
	v.SetDefault("cache.default_ttl", "5m")
	v.SetDefault("cache.embeddings_ttl", "60m")
	v.SetDefault("cache.max_body_size", 10485760)
	// Register the optional cache.redis block so CL_CACHE_REDIS_* env vars are
	// honored in env-only deployments (AutomaticEnv only resolves known keys).
	// Empty host (the default) means "share the main redis" — InitCacheRedis
	// treats a nil block or an empty host identically.
	v.SetDefault("cache.redis.host", "")
	v.SetDefault("cache.redis.port", 6379)
	v.SetDefault("cache.redis.password", "")
	v.SetDefault("cache.redis.db", 0)
	v.SetDefault("cache.redis.pool_size", 200)
	v.SetDefault("cache.redis.min_idle_conns", 20)

	v.SetDefault("secret_manager.cache_ttl", "5m")

	v.SetDefault("guardrail_alert.enabled", true)
	v.SetDefault("guardrail_alert.concurrency", 8)
	v.SetDefault("guardrail_alert.content_preview", true)
	v.SetDefault("guardrail_alert.content_preview_len", 200)

	v.SetDefault("license.heartbeat_enabled", false)
	v.SetDefault("license.heartbeat_interval", "24h")
	v.SetDefault("license.verify_mode", "off")
	v.SetDefault("license.verify_issuer", "")
	v.SetDefault("license.verify_audience", "")
	v.SetDefault("license.verify_keys", "")

	// U-06: viper's AutomaticEnv only resolves REGISTERED keys — these were
	// documented as env-overridable (config.example.yaml) but silently
	// ignored in pure-env deployments. Empty-string defaults keep the YAML
	// path unchanged; CL_SMTP_*/CL_LICENSE_* now actually work.
	v.SetDefault("smtp.host", "")
	v.SetDefault("smtp.port", 587)
	v.SetDefault("smtp.username", "")
	v.SetDefault("smtp.password", "")
	v.SetDefault("smtp.from", "")
	v.SetDefault("license.server_url", "")
	v.SetDefault("license.license_key", "")
	v.SetDefault("license.license_file", "")
	v.SetDefault("license.offline_file", "")
	v.SetDefault("license.node_id", "")

	v.SetDefault("mcp.enabled", true)
	v.SetDefault("mcp.max_servers", 0)
	v.SetDefault("mcp.health_check_interval", "30s")
	v.SetDefault("mcp.tool_cache_ttl", "5m")
	v.SetDefault("mcp.request_timeout", "30s")
	v.SetDefault("mcp.http_max_idle_conns", 10)
	v.SetDefault("mcp.rate_limit_enabled", true)
	v.SetDefault("mcp.rate_limit_default_rpm", 60)
	v.SetDefault("mcp.log_retention_days", 180)

	v.SetDefault("crypto.mode", "standard")

	v.SetDefault("datalens.enabled", true)
	v.SetDefault("datalens.retention.raw_logs_days", 365)
	v.SetDefault("datalens.retention.hourly_metrics_days", 90)
	v.SetDefault("datalens.retention.daily_metrics_days", 730)
	v.SetDefault("datalens.aggregation.interval", "1h")
	v.SetDefault("datalens.aggregation.lookback", "3h")
	v.SetDefault("datalens.aggregation.backfill_days", 7)
	v.SetDefault("datalens.aggregation.backfill_concurrency", 1)
	v.SetDefault("datalens.aggregation.levels", []string{"global", "by_model", "by_team", "by_provider", "by_key", "team_model", "key_model"})
	v.SetDefault("datalens.from_name", "CrossLink DataLens")

	// Allow both short and long env var names for the encryption key
	v.BindEnv("secret_manager.encryption_key", "CL_ENCRYPTION_KEY", "CL_SECRET_MANAGER_ENCRYPTION_KEY")
}

// WriteLicenseKey updates the license.license_key field in the YAML config file.
// It preserves all other content in the file.
func WriteLicenseKey(key string) error {
	path := configFilePath
	if path == "" {
		path = findConfigFile()
	}
	if path == "" {
		return fmt.Errorf("config file not found")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	lic, _ := raw["license"].(map[string]any)
	if lic == nil {
		lic = make(map[string]any)
	}
	lic["license_key"] = key
	raw["license"] = lic

	out, err := yaml.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(path, out, 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func findConfigFile() string {
	for _, p := range []string{"configs/config.yaml", "config.yaml"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
