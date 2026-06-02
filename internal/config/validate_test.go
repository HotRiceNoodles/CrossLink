package config

import (
	"strings"
	"testing"
	"time"
)

func validConfig() *Config {
	return &Config{
		Server:    ServerConfig{Port: 8080, ReadTimeout: 30 * time.Second, WriteTimeout: 120 * time.Second},
		Database:  DatabaseConfig{Driver: "postgres", Host: "localhost", Port: 5432, User: "user", Password: "pass", DBName: "testdb", SSLMode: "disable"},
		Redis:     RedisConfig{Host: "localhost", Port: 6379, Password: "", DB: 0},
		Admin:     AdminConfig{Username: "admin", Password: "changeme", JWTSecret: "a-very-long-secret-that-is-at-least-32-chars", TokenExpiry: 24},
		RateLimit: RateLimitConfig{RPM: 60, TPM: 100000},
		Logging:   LoggingConfig{Level: "info", Format: "json"},
		Cache:     CacheConfig{DefaultTTL: 5 * time.Minute},
	}
}

func TestValidate_ValidConfig(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid config should pass, got: %v", err)
	}
}

func TestValidate_ServerPort(t *testing.T) {
	tests := []struct {
		name string
		port int
		ok   bool
	}{
		{"zero", 0, false},
		{"negative", -1, false},
		{"too high", 70000, false},
		{"one", 1, true},
		{"8080", 8080, true},
		{"65535", 65535, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Server.Port = tt.port
			err := cfg.Validate()
			if (err == nil) != tt.ok {
				t.Errorf("port=%d: err=%v, want ok=%v", tt.port, err, tt.ok)
			}
		})
	}
}

func TestValidate_Database(t *testing.T) {
	t.Run("empty host", func(t *testing.T) {
		cfg := validConfig()
		cfg.Database.Host = ""
		if err := cfg.Validate(); err == nil {
			t.Error("empty host should fail")
		}
	})
	t.Run("empty dbname", func(t *testing.T) {
		cfg := validConfig()
		cfg.Database.DBName = ""
		if err := cfg.Validate(); err == nil {
			t.Error("empty dbname should fail")
		}
	})
	t.Run("invalid port", func(t *testing.T) {
		cfg := validConfig()
		cfg.Database.Port = -1
		if err := cfg.Validate(); err == nil {
			t.Error("port -1 should fail")
		}
	})
	t.Run("invalid sslmode", func(t *testing.T) {
		cfg := validConfig()
		cfg.Database.SSLMode = "invalid"
		if err := cfg.Validate(); err == nil {
			t.Error("invalid sslmode should fail")
		}
	})
	t.Run("valid sslmodes", func(t *testing.T) {
		for _, mode := range []string{"disable", "require", "verify-ca", "verify-full", "allow", "prefer"} {
			cfg := validConfig()
			cfg.Database.SSLMode = mode
			if err := cfg.Validate(); err != nil {
				t.Errorf("sslmode=%q should pass, got: %v", mode, err)
			}
		}
	})
}

func TestValidate_Redis(t *testing.T) {
	t.Run("db too high", func(t *testing.T) {
		cfg := validConfig()
		cfg.Redis.DB = 16
		if err := cfg.Validate(); err == nil {
			t.Error("redis.db=16 should fail")
		}
	})
	t.Run("db negative", func(t *testing.T) {
		cfg := validConfig()
		cfg.Redis.DB = -1
		if err := cfg.Validate(); err == nil {
			t.Error("redis.db=-1 should fail")
		}
	})
	t.Run("invalid port", func(t *testing.T) {
		cfg := validConfig()
		cfg.Redis.Port = 0
		if err := cfg.Validate(); err == nil {
			t.Error("redis port 0 should fail")
		}
	})
}

func TestValidate_RateLimit(t *testing.T) {
	t.Run("negative rpm", func(t *testing.T) {
		cfg := validConfig()
		cfg.RateLimit.RPM = -1
		if err := cfg.Validate(); err == nil {
			t.Error("negative rpm should fail")
		}
	})
	t.Run("negative tpm", func(t *testing.T) {
		cfg := validConfig()
		cfg.RateLimit.TPM = -1
		if err := cfg.Validate(); err == nil {
			t.Error("negative tpm should fail")
		}
	})
	t.Run("zero rpm ok", func(t *testing.T) {
		cfg := validConfig()
		cfg.RateLimit.RPM = 0
		if err := cfg.Validate(); err != nil {
			t.Errorf("rpm=0 should pass, got: %v", err)
		}
	})
}

func TestValidate_TokenExpiry(t *testing.T) {
	cfg := validConfig()
	cfg.Admin.TokenExpiry = 0
	if err := cfg.Validate(); err == nil {
		t.Error("token_expiry=0 should fail")
	}
}

func TestValidate_Logging(t *testing.T) {
	t.Run("invalid level", func(t *testing.T) {
		cfg := validConfig()
		cfg.Logging.Level = "verbose"
		if err := cfg.Validate(); err == nil {
			t.Error("invalid log level should fail")
		}
	})
	t.Run("invalid format", func(t *testing.T) {
		cfg := validConfig()
		cfg.Logging.Format = "xml"
		if err := cfg.Validate(); err == nil {
			t.Error("invalid log format should fail")
		}
	})
}

func TestValidate_CacheTTL(t *testing.T) {
	cfg := validConfig()
	cfg.Cache.DefaultTTL = -1 * time.Second
	if err := cfg.Validate(); err == nil {
		t.Error("negative cache ttl should fail")
	}
}

func TestValidate_DatabaseDriver(t *testing.T) {
	t.Run("sqlite without path", func(t *testing.T) {
		cfg := validConfig()
		cfg.Database.Driver = "sqlite"
		cfg.Database.SQLitePath = ""
		if err := cfg.Validate(); err == nil {
			t.Error("sqlite without sqlite_path should fail")
		}
	})
	t.Run("sqlite with path", func(t *testing.T) {
		cfg := validConfig()
		cfg.Database.Driver = "sqlite"
		cfg.Database.SQLitePath = "/tmp/test.db"
		// This will pass database validation but may fail on Redis;
		// we only check that no database-related error occurs.
		err := cfg.Validate()
		if err != nil {
			// Should NOT be a database error
			if strings.Contains(err.Error(), "database.") {
				t.Errorf("sqlite with valid path should not have database error, got: %v", err)
			}
		}
	})
	t.Run("unknown driver", func(t *testing.T) {
		cfg := validConfig()
		cfg.Database.Driver = "unknown"
		if err := cfg.Validate(); err == nil {
			t.Error("unknown driver should fail")
		}
	})
	t.Run("empty driver defaults to postgres", func(t *testing.T) {
		cfg := validConfig()
		cfg.Database.Driver = ""
		if err := cfg.Validate(); err != nil {
			t.Errorf("empty driver should default to postgres and pass, got: %v", err)
		}
		if cfg.Database.Driver != "postgres" {
			t.Errorf("empty driver should be normalized to postgres, got: %q", cfg.Database.Driver)
		}
	})
	t.Run("mysql valid config", func(t *testing.T) {
		cfg := validConfig()
		cfg.Database.Driver = "mysql"
		cfg.Database.Port = 3306
		if err := cfg.Validate(); err != nil {
			t.Errorf("valid mysql config should pass, got: %v", err)
		}
	})
	t.Run("mysql without host", func(t *testing.T) {
		cfg := validConfig()
		cfg.Database.Driver = "mysql"
		cfg.Database.Host = ""
		if err := cfg.Validate(); err == nil {
			t.Error("mysql without host should fail")
		}
	})
	t.Run("mysql without dbname", func(t *testing.T) {
		cfg := validConfig()
		cfg.Database.Driver = "mysql"
		cfg.Database.DBName = ""
		if err := cfg.Validate(); err == nil {
			t.Error("mysql without dbname should fail")
		}
	})
	t.Run("mysql port zero defaults to 3306", func(t *testing.T) {
		cfg := validConfig()
		cfg.Database.Driver = "mysql"
		cfg.Database.Port = 0
		if err := cfg.Validate(); err != nil {
			t.Errorf("mysql port=0 should default to 3306, got: %v", err)
		}
		if cfg.Database.Port != 3306 {
			t.Errorf("mysql port should be 3306, got: %d", cfg.Database.Port)
		}
	})
}
