# SQLite migrations — EXPERIMENTAL

SQLite is an **experimental** backend. Only the initial schema (`000001`) is
applied — the runner in `internal/dialect/sqlite.go` does not use
golang-migrate and ignores everything after the first migration. A SQLite
database will be missing most tables and is suitable for evaluation only.

The server prints a warning at startup when running against SQLite.

Do not add new migrations to this directory unless the dialect is being
re-adopted (full port of the PostgreSQL set + CI coverage). See
[docs/migration-guidelines.md](../../docs/migration-guidelines.md).
