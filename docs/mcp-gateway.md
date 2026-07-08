# MCP Gateway

CrossLink includes a full **Model Context Protocol** gateway — not a thin proxy. It fronts upstream MCP servers with transport abstraction, per-tool access control, encrypted credentials, health checks, and structured call logging.

## Transport abstraction

MCP servers are registered with a transport type (`internal/mcp/transport_sse.go` and siblings):

- **HTTP** — standard JSON-RPC over HTTP.
- **SSE** — Server-Sent Events transport. CrossLink auto-discovers the upstream SSE endpoint and validates same-origin to prevent SSRF.
- **stdio** (pluggable) — for local-tool transports.

Connections are pooled via a shared `http.Transport` with configurable max idle conns per host, so a high-volume of tool calls doesn't open a connection per request.

## Tool discovery

Tool lists are fetched from each upstream and **cached with singleflight dedup** (`internal/mcp/service.go`): if 50 requests ask for the tool list simultaneously while the cache is cold, only one upstream call is made. `GET /admin/api/mcp/servers/:id/tools` reads from this cache.

## Per-tool RBAC

Every `tools/call` is checked against a permission matrix (`internal/mcp/handler.go`) before forwarding:

- Permissions are scoped by **principal** — API key, team, or role.
- Each tool can be on the **allow** or **deny** list for a principal.
- Deny wins: a tool on both lists is denied.

This lets you expose a single MCP server to multiple tenants with different tool surfaces — e.g., a read-only key can call `search` but not `delete`.

## Encrypted credentials

Upstream MCP server credentials (API keys, tokens) are not stored as plaintext. They're stored as **URI references** resolved at runtime by `internal/secret/resolver.go`:

| Scheme | Source |
|--------|--------|
| `env://VAR_NAME` | Environment variable |
| `enc://<blob>` | Encrypted blob in the DB store (`internal/secret/store_encrypted.go`) |
| `db://...` | DB-backed reference |

Sensitive fields (`api_key`, `secret_access_key`, …) are auto-detected and encrypted at rest. A background watcher handles key rotation.

## Health checks

Each registered MCP server is health-checked on a configurable interval (`mcp.health_check_interval`, default 30s). Checks run in parallel with a concurrency semaphore (sem=10), so a single slow upstream doesn't stall the health-check sweep. Unhealthy servers are flagged and skipped by the router.

## Call logging

Every tool call is logged asynchronously via a **channel-based worker** with batched writes and per-task panic recovery:

- Logs are **partitioned by month** for query efficiency on large deployments.
- An auto-cleanup job drops partitions older than the retention window.

The async channel means tool-call latency isn't padded by log-write latency — the response returns to the client as soon as the upstream replies, and the log write happens off the hot path.
