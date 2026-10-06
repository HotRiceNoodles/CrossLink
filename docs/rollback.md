# Rollback Guide

How to recover from a bad CrossLink release.

## Principle

Rollback = **binary rollback first, schema rollback rarely**. Because
migrations follow expand-contract ([migration-guidelines.md](migration-guidelines.md)),
rolling the binary back one release is always schema-safe: the older code
tolerates the newer (expanded) schema.

## Procedure

1. **Pin the previous release.** With pre-built images:
   ```yaml
   # docker-compose.prod.yaml
   image: ghcr.io/hotricenoodles/crosslink:v0.1.3   # instead of :latest
   ```
   Or rebuild from the previous git tag (`git checkout vX.Y.Z-1`).
2. **Restart.** The old binary starts against the newer schema — migrations
   already recorded in `schema_migrations` are skipped; expand-phase tables and
   nullable columns are ignored by the old code.
3. **Verify** via `/health`, `/admin/api/version`, and gateway smoke calls.

## When to also roll back schema

Only when the offending migration causes the *old* binary to fail (contract
migration ran while old code still deployed, or a broken migration itself):

```bash
# Drop one migration (requires downtime window — down may lose data added since up)
migrate -database "$DATABASE_URL" -path migrations/postgres down 1
```

- `down` scripts are verified in CI but may still **lose data** (columns
  dropped in the up direction cannot be recovered). Take a backup first.
- Rolling back more than one step across a contract boundary is
  **unsupported** — restore from a snapshot instead.

## Support boundary

| Scenario | Supported |
|---|---|
| Binary rollback over expand-phase schema | ✅ always |
| Single `down` with backup taken | ⚠️ manual, data loss possible |
| Multi-step down across contract boundary | ❌ restore from DB snapshot |

## MySQL / SQLite

Experimental dialects whose migrations stop at `000002`; see the warning in
`migrations/mysql/README.md`. Rollback for them is out of scope.
