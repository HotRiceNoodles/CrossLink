# API Versioning Policy

How CrossLink versions its API surfaces and manages breaking changes.

## Surfaces

| Surface | Path prefix | Version |
|---|---|---|
| Gateway API (OpenAI/Anthropic compatible) | `/v1` | `v1` |
| Admin API | `/admin/api` | `v1` (header-negotiated, no path version) |
| Self-service/portal | `/v1/*`, `/portal/api/*` | `v1` |
| MCP gateway | `/mcp` | MCP protocol `1.0` |

The current surface versions are served at `GET /admin/api/version` under the
`api` key (build metadata is separate).

## Why `/v1` is fixed (no `/v2` reservation)

The gateway API must stay wire-compatible with the OpenAI (`/v1/chat/completions`)
and Anthropic (`/v1/messages`) protocols — clients hardcode these paths in SDKs.
CrossLink therefore never introduces `/v2` for protocol endpoints; evolution
happens through:

1. **Additive-only changes within `v1`** — new endpoints, new optional request
   fields, new response fields. Clients must ignore response fields they don't
   recognize, and the gateway must not reject requests containing unknown
   fields.
2. **Endpoint-level deprecation with Sunset** when a route must eventually go
   away.

If a future protocol shift truly requires a new major surface, `/v2` will be
introduced with at least 12 months of overlap, announced a release cycle in
advance.

## Deprecation process

1. **Register** the route in `internal/apidoc/deprecations.go`
   (`ActiveDeprecations`) — include announcement date, sunset date, successor,
   reason. The registry is the source of truth; it is compiled into the binary.
2. **Headers are served automatically** by the `middleware.APIVersion` chain on
   affected routes:
   - `X-API-Version: v1` — on every gateway response
   - `Deprecation: true` — RFC 9745, on deprecated routes
   - `Sunset: <HTTP-date>` — RFC 8594, removal date
   - `Link: <successor>; rel="successor-version"` — when a replacement exists
3. **Announce** in the release CHANGELOG (`Deprecated` section).
4. **Remove** the route and its registry entry together in the first release
   after the sunset date.

### Sunset periods

- While the project is `0.x`: **12 months** minimum between announcement and
  removal.
- After `1.0`: **6 months** minimum.

## Compatibility commitments (within `v1`)

- Adding request/response fields: allowed (clients must tolerate unknowns).
- Removing or renaming fields, changing types, changing error semantics:
  **breaking** — requires deprecation.
- Renaming/removing endpoints: **breaking** — requires deprecation.
- New endpoints: always allowed.
- Admin API (`/admin/api`): follows the same rules. It has no path version;
  its version is reported in `/admin/api/version` (`api.admin`).

## OpenAPI spec

`internal/apidoc/openapi.bundled.json` is the published spec (served at
`GET /openapi.json`). `info.version` tracks the release version. Deprecated
endpoints carry an `x-sunset` extension mirroring the registry.

## For operators

`GET /admin/api/deprecations` lists all active deprecations (announcement date,
sunset date, successor) — query it before planning an upgrade. The same
information is visible per-request via the `Deprecation`/`Sunset` headers.
