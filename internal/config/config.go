package config

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig    `mapstructure:"server"`
	Database  DatabaseConfig  `mapstructure:"database"`
	Redis     RedisConfig     `mapstructure:"redis"`
	Gateway   GatewayConfig   `mapstructure:"gateway"`
	Admin     AdminConfig     `mapstructure:"admin"`
	RateLimit RateLimitConfig `mapstructure:"rate_limit"`
	Logging   LoggingConfig   `mapstructure:"logging"`
	Cache          CacheConfig          `mapstructure:"cache"`
	SecretManager  SecretManagerConfig  `mapstructure:"secret_manager"`
	SMTP           SMTPConfig           `mapstructure:"smtp"`
	GuardrailAlert GuardrailAlertConfig `mapstructure:"guardrail_alert"`
	CORS           CORSConfig           `mapstructure:"cors"`
	License        LicenseConfig        `mapstructure:"license"`
	MCP            MCPConfig            `mapstructure:"mcp"`
	Crypto         CryptoConfig         `mapstructure:"crypto"`
}

type ServerConfig struct {
	Port            int           `mapstructure:"port"`
	ReadTimeout     time.Duration `mapstructure:"read_timeout"`
	WriteTimeout    time.Duration `mapstructure:"write_timeout"`
	TrustedProxies  []string      `mapstructure:"trusted_proxies"`
}

type DatabaseConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`
	DBName   string `mapstructure:"dbname"`
	SSLMode  string `mapstructure:"sslmode"`
}

func (d DatabaseConfig) DSN() string {
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		d.Host, d.Port, d.User, d.Password, d.DBName, d.SSLMode)
}

func (d DatabaseConfig) DSNURL() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		d.User, d.Password, d.Host, d.Port, d.DBName, d.SSLMode)
}

type RedisConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

func (r RedisConfig) Addr() string {
	return fmt.Sprintf("%s:%d", r.Host, r.Port)
}

type GatewayConfig struct {
	AuthKey string `mapstructure:"auth_key"`
	BaseURL string `mapstructure:"base_url"`
}

type AdminConfig struct {
	Username     string `mapstructure:"username"`
	Password     string `mapstructure:"password"`
	PasswordHash string `mapstructure:"-"`
	JWTSecret    string `mapstructure:"jwt_secret"`
	TokenExpiry  int    `mapstructure:"token_expiry"`
}

type RateLimitConfig struct {
	RPM int `mapstructure:"rpm"`
	TPM int `mapstructure:"tpm"`
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
	Enabled          bool `mapstructure:"enabled"`
	Concurrency      int  `mapstructure:"concurrency"`
	ContentPreview   bool `mapstructure:"content_preview"`
	ContentPreviewLen int `mapstructure:"content_preview_len"`
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
}

type CryptoConfig struct {
	Mode string `mapstructure:"mode"` // "standard" (default) or "gm"
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
	if c.Database.Host == "" {
		return fmt.Errorf("database.host is required")
	}
	if c.Database.Port < 1 || c.Database.Port > 65535 {
		return fmt.Errorf("database.port must be between 1 and 65535, got %d", c.Database.Port)
	}
	if c.Database.DBName == "" {
		return fmt.Errorf("database.dbname is required")
	}
	if c.Database.SSLMode != "" && !validSSLModes[c.Database.SSLMode] {
		return fmt.Errorf("database.sslmode invalid: %q", c.Database.SSLMode)
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
	return nil
}

func (a AdminConfig) IsDefaultJWTSecret() bool {
	return a.JWTSecret == "" || a.JWTSecret == "change-me-to-a-random-secret" || a.JWTSecret == "dev-secret-change-in-production"
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
	v.AutomaticEnv()

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
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.read_timeout", "30s")
	v.SetDefault("server.write_timeout", "120s")

	v.SetDefault("database.host", "localhost")
	v.SetDefault("database.port", 5432)
	v.SetDefault("database.user", "crosslink")
	v.SetDefault("database.password", "")
	v.SetDefault("database.dbname", "crosslink")
	v.SetDefault("database.sslmode", "disable")

	v.SetDefault("redis.host", "localhost")
	v.SetDefault("redis.port", 6379)
	v.SetDefault("redis.password", "")
	v.SetDefault("redis.db", 0)

	v.SetDefault("admin.username", "admin")
	v.SetDefault("admin.password", "changeme")
	v.SetDefault("admin.jwt_secret", "change-me-to-a-random-secret")
	v.SetDefault("admin.token_expiry", 24)

	v.SetDefault("rate_limit.rpm", 60)
	v.SetDefault("rate_limit.tpm", 100000)

	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "json")

	v.SetDefault("cache.enabled", true)
	v.SetDefault("cache.default_ttl", "5m")
	v.SetDefault("cache.embeddings_ttl", "60m")
	v.SetDefault("cache.max_body_size", 10485760)

	v.SetDefault("secret_manager.cache_ttl", "5m")

	v.SetDefault("guardrail_alert.enabled", true)
	v.SetDefault("guardrail_alert.concurrency", 8)
	v.SetDefault("guardrail_alert.content_preview", true)
	v.SetDefault("guardrail_alert.content_preview_len", 200)

	v.SetDefault("license.heartbeat_enabled", false)
	v.SetDefault("license.heartbeat_interval", "24h")

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

	if err := os.WriteFile(path, out, 0644); err != nil {
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
