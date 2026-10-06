# Database Migration Guidelines

Rules for the golang-migrate SQL files under `migrations/`. Migrations run
automatically at startup (advisory-locked: `pg_advisory_lock(20260518)` on
PostgreSQL/Kingbase, `GET_LOCK` on MySQL, file lock on SQLite), so every
migration must be safe to apply by a rolling fleet.

## Hard rules

1. **Sequential numbering only.** File names are `NNNNNN_snake_name.{up,down}.sql`
   with a 6-digit sequence number (next free: `000075`). Numbers are append-only:
   never reuse, renumber, or insert before an existing migration.
2. **Never delete or edit a merged migration.** If a migration turns out wrong,
   add a new one that corrects it. History is immutable once on `main`.
3. **up/down must come in pairs**, and `down` must be genuinely executable —
   it is verified in CI (full `up` → `down` → `up` cycle), not decoration.
   A `down` that drops a whole table to undo an `ALTER` is rejected in review.
4. **One migration per PR.** Migration PRs are labeled `migration` and kept
   separate from feature code so they can be cherry-picked or reverted
   independently.
5. **No destructive change in one step.** Renames, type changes, and column
   drops follow the expand-contract pattern below.

## Expand-contract (two-phase) changes

To keep "rollback the binary one release" always possible:

- **Expand** (release N): add the new column/table/index, dual-write if
  applicable. Release N's code must tolerate both old and new shapes.
- **Contract** (release N+1 or later): after N is fully deployed, add a
  migration that removes the old shape.

Contract migrations must reference the minimum release that survives them in
the PR description (e.g. "safe to run only ≥ v0.4.0").

## Writing migrations

- Target the **PostgreSQL** dialect first (`migrations/postgres/`); Kingbase
  reuses those files verbatim.
- **MySQL/SQLite are experimental** — see the warning in each directory's
  README; do not add new migrations there without explicitly re-adopting those
  dialects (see `docs/rollback.md` and README support matrix).
- Large-table changes: state the expected lock impact in the PR (e.g. PG < 11
  rewrites the table when adding a column with a default; prefer adding
  `NULL` then backfilling in batches).
- Keep migrations idempotent-ish where cheap (`IF NOT EXISTS`), though
  golang-migrate's version tracking makes strictness optional.
- Never reference data that only exists in one environment (IDs, tenant names).

## CI enforcement

- `scripts/check-migrations.sh` validates structure: up/down pairing, no
  duplicate or out-of-order sequence numbers, no shadowing of merged files.
- The `migration-check` CI job runs the full PostgreSQL set `up` → `down` →
  `up` on an ephemeral database to prove `down` files actually work.

## Local checks

```bash
bash scripts/check-migrations.sh
docker compose -f deployments/docker-compose.dev.yaml up -d postgres
# point the server at it once, or use the migrate CLI:
migrate -database "postgres://crosslink:crosslink@localhost:5432/crosslink?sslmode=disable" -path migrations/postgres up
```
