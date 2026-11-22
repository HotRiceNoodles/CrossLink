package dialect

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
)

// stubDriver implements the minimum migrate.Driver surface needed by
// Migrate.Version(); Run/Drop are never invoked because skipForeignSchemaVersion
// only reads state.
type stubDriver struct {
	version uint
	dirty   bool
	err     error
}

func (d *stubDriver) Close() error                             { return nil }
func (d *stubDriver) Open(url string) (database.Driver, error) { return d, nil }
func (d *stubDriver) Lock() error                              { return nil }
func (d *stubDriver) Unlock() error                            { return nil }
func (d *stubDriver) Run(io.Reader) error                      { return nil }
func (d *stubDriver) Drop() error                              { return nil }
func (d *stubDriver) SetVersion(version int, dirty bool) error { return nil }
func (d *stubDriver) Version() (int, bool, error) {
	if d.err != nil {
		return 0, false, d.err
	}
	return int(d.version), d.dirty, nil
}

func writeChain(t *testing.T, versions ...uint) string {
	t.Helper()
	dir := t.TempDir()
	for _, v := range versions {
		base := filepath.Join(dir, migrateVersionName(v))
		if err := os.WriteFile(base+".up.sql", []byte("SELECT 1;"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(base+".down.sql", []byte("SELECT 1;"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func migrateVersionName(v uint) string {
	return fmt.Sprintf("%06d_test", v) // real files are <6-digit version>_<name>.sql
}

func TestSkipForeignSchemaVersion(t *testing.T) {
	t.Parallel()
	// Chain provides 1 and 105, mirroring a community build whose tail is 105.
	dir := writeChain(t, 1, 105)

	tests := []struct {
		name    string
		version uint
		dirty   bool
		err     error
		skip    bool
	}{
		{name: "version in chain", version: 105, skip: false},
		{name: "version ahead of chain (higher edition migrated)", version: 110, skip: true},
		{name: "version below chain gap", version: 3, skip: true},
		{name: "dirty state defers to Up", version: 110, dirty: true, skip: false},
		{name: "fresh database defers to Up", version: 0, err: migrate.ErrNilVersion, skip: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, err := migrate.NewWithDatabaseInstance("file://"+filepath.ToSlash(dir), "stub", &stubDriver{version: tc.version, dirty: tc.dirty, err: tc.err})
			if err != nil {
				t.Fatalf("create migrator: %v", err)
			}
			skip, err := skipForeignSchemaVersion(m, dir)
			if err != nil {
				t.Fatalf("skipForeignSchemaVersion: %v", err)
			}
			if skip != tc.skip {
				t.Fatalf("skip = %v, want %v", skip, tc.skip)
			}
		})
	}
}

func TestSourceVersions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	files := map[string]string{
		"000001_init.up.sql":   "",
		"000105_tail.up.sql":   "",
		"000105_tail.down.sql": "", // down files must not add versions
		"README.md":            "", // non-migration files ignored
		"notanumber_x.up.sql":  "", // unparseable prefix ignored
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := sourceVersions(dir)
	if err != nil {
		t.Fatalf("sourceVersions: %v", err)
	}
	want := map[uint]bool{1: true, 105: true}
	if len(got) != len(want) {
		t.Fatalf("versions = %v, want %v", got, want)
	}
	for v := range want {
		if !got[v] {
			t.Fatalf("versions missing %d: %v", v, got)
		}
	}

	if _, err := sourceVersions(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("sourceVersions should fail on a missing directory")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected error: %v", err)
	}
}
