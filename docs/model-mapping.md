# Model Mapping Governance

How model routing mappings (public model name → provider/upstream model) are
versioned and changed safely.

## Where mappings live

Mappings are database rows, not config files:

- `providers` table (`internal/model/provider.go`) — upstream provider
  instances (base URL, credentials, adapter type).
- `provider_models` table (`internal/model/provider_model.go`) — the routing
  rows: public `model_name` → `provider_model` on a specific `provider_id`,
  with weight, priority, status, pricing, and routing strategy.

Because they are schema-backed, mapping **structure** changes go through the
normal migration process ([migration-guidelines.md](migration-guidelines.md)).
This document governs mapping **data** changes.

## Change rules

1. **Changes go through the Admin API only** (`/admin/api/models`,
   `/admin/api/providers`), never by hand-editing the database. The admin
   handlers run RBAC (`RequireAction`) and feed the audit trail; direct DB
   edits bypass both and are invisible to rollback forensics.
2. **Audit trail is mandatory.** In commercial builds the audit service records
   who changed which mapping and when. Community builds have no audit sink —
   treat `updated_at` plus admin judgment accordingly, and prefer disabling a
   mapping (`status`) over deleting it so history remains queryable.
3. **Additive-first.** Adding a new mapping or a new provider for an existing
   public model is low-risk (traffic shifts by weight). Renaming a public
   `model_name` breaks every client using it — treat it like a breaking API
   change and follow the [deprecation process](api-versioning.md) instead.
4. **Pricing changes are money-relevant.** `input_price`/`output_price`
   alterations affect billing reconciliation; change them alongside a usage
   report cutoff, not mid-day.

## Adapter versions

Each provider adapter registers an `AdapterVersion`
(`internal/provider/adapter_registry.go`) that is independent of the gateway
release version — bump it whenever the adapter's wire behavior changes
(translation, auth, retry semantics). Active adapter versions are visible at
`GET /admin/api/adapters` (`meta.adapter_version`), which makes
"same gateway version, different provider behavior" diagnosable from system
info without grepping source.

## Rollback

Mapping changes are data, so rollback is a data operation: re-apply the
previous values via the Admin API (or restore from DB snapshot). Disable-bad-
mapping (`status = 0`) is the fast path for a misbehaving upstream — the
fallback engine picks the remaining candidates without a restart.
