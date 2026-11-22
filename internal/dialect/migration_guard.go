package dialect

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/golang-migrate/migrate/v4"
)

// skipForeignSchemaVersion reports whether the database's recorded schema
// version does not exist in this build's migration chain — e.g. a shared
// database previously migrated by a higher edition whose chain contains
// extra migrations. golang-migrate's Up() aborts startup in that case
// ("no migration found for version N"), so the caller skips migration and
// starts normally, leaving the unknown newer tables untouched. Only the
// build owning those migrations can take the schema further.
func skipForeignSchemaVersion(m *migrate.Migrate, migrationDir string) (bool, error) {
	version, dirty, err := m.Version()
	if err != nil || dirty {
		return false, nil // fresh, unreadable, or dirty state: let Up() report it
	}
	versions, err := sourceVersions(migrationDir)
	if err != nil {
		return false, err
	}
	if versions[version] {
		return false, nil
	}
	slog.Warn("database schema version is not in this build's migration chain; skipping migrations",
		"db_version", version,
		"migration_dir", migrationDir,
		"hint", "database was likely migrated by a higher edition build; only that build can migrate it further")
	return true, nil
}

// sourceVersions parses the *.up.sql filenames in migrationDir into the set
// of versions the chain provides. Files whose numeric prefix does not parse
// (README, etc.) are ignored.
func sourceVersions(migrationDir string) (map[uint]bool, error) {
	entries, err := os.ReadDir(migrationDir)
	if err != nil {
		return nil, fmt.Errorf("read migration dir: %w", err)
	}
	versions := make(map[uint]bool, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		v, err := strconv.ParseUint(strings.SplitN(name, "_", 2)[0], 10, 32)
		if err != nil {
			continue
		}
		versions[uint(v)] = true
	}
	return versions, nil
}
