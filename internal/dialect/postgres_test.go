package dialect

import (
	"testing"
	"time"
)

func testPostgresDialect() *PostgresDialect {
	return NewPostgresDialect(DBConfig{
		Driver:   "postgres",
		Host:     "localhost",
		Port:     5432,
		User:     "testuser",
		Password: "testpass",
		DBName:   "testdb",
		SSLMode:  "disable",
	})
}

func TestPostgresDialect_Identity(t *testing.T) {
	p := testPostgresDialect()
	if got := p.Name(); got != "postgres" {
		t.Errorf("Name() = %q, want %q", got, "postgres")
	}
}

func TestPostgresDialect_Capabilities(t *testing.T) {
	p := testPostgresDialect()
	c := p.Capabilities()

	if !c.PartialIndex {
		t.Error("Capabilities().PartialIndex = false, want true")
	}
	if !c.AdvisoryLock {
		t.Error("Capabilities().AdvisoryLock = false, want true")
	}
	if c.ConcurrentWrites < 100 {
		t.Errorf("Capabilities().ConcurrentWrites = %d, want >= 100", c.ConcurrentWrites)
	}
}

func TestPostgresDialect_PoolConfig(t *testing.T) {
	p := testPostgresDialect()
	pool := p.PoolConfig()

	if pool.MaxOpenConns != 100 {
		t.Errorf("PoolConfig().MaxOpenConns = %d, want 100", pool.MaxOpenConns)
	}
	if pool.MaxIdleConns != 50 {
		t.Errorf("PoolConfig().MaxIdleConns = %d, want 50", pool.MaxIdleConns)
	}
	if pool.ConnMaxLifetime != 5*time.Minute {
		t.Errorf("PoolConfig().ConnMaxLifetime = %v, want %v", pool.ConnMaxLifetime, 5*time.Minute)
	}
	if pool.ConnMaxIdleTime != 1*time.Minute {
		t.Errorf("PoolConfig().ConnMaxIdleTime = %v, want %v", pool.ConnMaxIdleTime, 1*time.Minute)
	}
}

func TestPostgresDialect_PartitionSupport(t *testing.T) {
	p := testPostgresDialect()
	if got := p.PartitionSupport(); got != PartitionNative {
		t.Errorf("PartitionSupport() = %v, want %v", got, PartitionNative)
	}
}

func TestPostgresDialect_MigrationDir(t *testing.T) {
	p := testPostgresDialect()
	if got := p.MigrationDir(); got != "migrations/postgres" {
		t.Errorf("MigrationDir() = %q, want %q", got, "migrations/postgres")
	}
}

func TestPostgresDialect_DSNFormats(t *testing.T) {
	p := testPostgresDialect()

	wantDSN := "host=localhost port=5432 user=testuser password=testpass dbname=testdb sslmode=disable"
	if got := p.dsn(); got != wantDSN {
		t.Errorf("dsn() = %q, want %q", got, wantDSN)
	}

	wantURL := "postgres://testuser:testpass@localhost:5432/testdb?sslmode=disable"
	if got := p.dsnURL(); got != wantURL {
		t.Errorf("dsnURL() = %q, want %q", got, wantURL)
	}
}
