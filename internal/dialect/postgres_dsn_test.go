package dialect

import (
	"strings"
	"testing"
)

// F-bb: every postgres dial (boot open, pool, migration, advisory lock)
// carries connect_timeout so a silent accept-hold DB cannot hang BOOT —
// the dial fails in bounded time and the process exits with a clear error.
func TestPostgresDSN_ConnectTimeoutPresent(t *testing.T) {
	p := NewPostgresDialect(DBConfig{Host: "db", Port: 5432, User: "u", Password: "p", DBName: "d", SSLMode: "disable"})
	if !strings.Contains(p.dsn(), " connect_timeout=5") {
		t.Fatalf("dsn missing default connect_timeout: %s", p.dsn())
	}
	if !strings.Contains(p.dsnURL(), "connect_timeout=5") {
		t.Fatalf("dsnURL missing default connect_timeout: %s", p.dsnURL())
	}
	// CL_DB_CONNECT_TIMEOUT is read once at process start (production
	// semantics); a runtime override is not assertable via t.Setenv here.
}
