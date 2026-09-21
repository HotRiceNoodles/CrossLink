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

## Example: registering a remote web-search MCP server

The HTTP transport works with any stateless streamable-HTTP MCP endpoint. As a worked example, here is the You.com MCP server (web search, page-content extraction, cited research) registered through the admin API.

### Keyless (basic web search)

The `?profile=free` endpoint exposes the `you-search` tool without an API key, so `auth_type` stays `none`:

```bash
curl -X POST http://localhost:8080/admin/api/mcp/servers \
  -H "Authorization: Bearer <jwt> \
  -H "Content-Type: application/json" \
  -d '{
        "name": "youcom",
        "display_name": "You.com Web Search",
        "description": "Web search and page content extraction via the You.com MCP server",
        "transport_type": "http",
        "url": "https://api.you.com/mcp?profile=free",
        "auth_type": "none"
      }'
```

### Authenticated (full tool surface)

With a [You.com API key](https://you.com/platform/api-keys) (exported as `YDC_API_KEY`), use the standard endpoint and bearer auth. The `bearer_token` field is a known-sensitive key, so the value is encrypted at rest before storage (see [Encrypted credentials](#encrypted-credentials)):

```bash
curl -X POST http://localhost:8080/admin/api/mcp/servers \
  -H "Authorization: Bearer <jwt> \
  -H "Content-Type: application/json" \
  -d '{
        "name": "youcom",
        "display_name": "You.com Web Search",
        "description": "Web search and page content extraction via the You.com MCP server",
        "transport_type": "http",
        "url": "https://api.you.com/mcp",
        "auth_type": "bearer",
        "auth_config": { "bearer_token": "'"${YDC_API_KEY}"'" }
      }'
```

### Verify and call through the gateway

```bash
# confirm the server is healthy and see its cached tool list
curl -X POST http://localhost:8080/admin/api/mcp/servers/1/test \
  -H "Authorization: Bearer cl-your-api-key
curl http://localhost:8080/admin/api/mcp/servers/1/tools \
  -H "Authorization: Bearer cl-your-api-key

# forward a JSON-RPC tool call through the gateway
curl -X POST http://localhost:8080/mcp/youcom \
  -H "Authorization: Bearer cl-your-api-key \
  -H "Content-Type: application/json" \
  -d '{
        "jsonrpc": "2.0",
        "id": 1,
        "method": "tools/call",
        "params": {
          "name": "you-search",
          "arguments": { "query": "streamable http mcp transport" }
        }
      }'
```

The tool call is forwarded with `Accept: application/json, text/event-stream`, and SSE-formatted replies are parsed back into a single JSON-RPC response (`internal/mcp/transport_http.go`), so both response shapes work. Registration is entirely opt-in: nothing is pre-registered, and any other remote MCP server can be added the same way.
