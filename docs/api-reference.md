# API Reference

CrossLink exposes three groups of endpoints: the **gateway** (OpenAI/Anthropic-compatible, API-key auth), the **MCP gateway** (JSON-RPC/SSE, API-key auth), and the **admin API** (JWT auth, used by the dashboard).

All gateway endpoints accept either an OpenAI-format or Anthropic-format request body and return the matching format. Protocol translation — including streaming SSE, tool use, and extended thinking — is handled internally.

## Authentication

| Scope | Header | Format |
|-------|--------|--------|
| Gateway & MCP | `Authorization: Bearer <key>` | API key prefixed `cl-` (48 hex chars) |
| Admin | `Authorization: Bearer <jwt>` | JWT obtained from `POST /admin/api/auth/login` |

API keys are stored hashed (never plaintext). The `cl-` prefix lets clients fail fast on malformed keys.

---

## Gateway endpoints

### `POST /v1/chat/completions`

OpenAI-compatible chat completions. Supports both streaming (`"stream": true`) and non-streaming.

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer cl-your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-chat",
    "stream": true,
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

### `POST /v1/messages`

Anthropic-compatible messages. Same routing and failover behavior as the OpenAI endpoint; the request and response are translated to/from Anthropic format.

```bash
curl http://localhost:8080/v1/messages \
  -H "Authorization: Bearer cl-your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4-20250514",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

### `GET /v1/models`

Lists the models available to the calling key (filtered by RBAC).

---

## Response headers

Every gateway response carries headers that make routing behavior observable:

| Header | When set | Meaning |
|--------|----------|---------|
| `x-crosslink-fallback-model` | Always (when a model is determined) | The model that actually served the request. Differs from the requested model when a cross-model fallback occurred. |
| `x-crosslink-fallback-count` | Only when at least one fallback happened | Number of providers attempted before success. |

Inspecting these lets you verify failover in real time — for example, logging them alongside latency to detect providers that are quietly degrading.

## Stream interruption

If an upstream SSE stream ends without a terminal `[DONE]` (network blip, upstream crash), CrossLink emits a structured error event instead of silently truncating:

```
data: {"error":{"type":"stream_interrupted","message":"upstream stream ended unexpectedly"}}
```

Clients get a machine-readable signal that the stream was incomplete, so they can retry rather than treating partial output as final.

## Error responses

Errors are returned in the format matching the request (OpenAI-style for `/v1/chat/completions`, Anthropic-style for `/v1/messages`). Common HTTP status codes:

| Status | Meaning |
|--------|---------|
| `400` | Malformed request body |
| `401` | Missing or invalid API key |
| `403` | RBAC denied (key lacks access to model/provider/MCP tool) |
| `429` | Rate limit (RPM/TPM) or concurrency limit hit |
| `502` | All providers in the fallback chain failed |
| `503` | Gateway is draining (during graceful shutdown) |

---

## MCP gateway endpoints

### `POST /mcp/:server`

Forwards a JSON-RPC request to the named MCP server. Tool calls are subject to per-principal RBAC (see [MCP Gateway](mcp-gateway.md)).

### `GET /mcp/:server`

SSE transport for the named MCP server.

---

## Admin endpoints

All admin endpoints require a JWT. The dashboard obtains it via login and refreshes it automatically.

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/admin/api/auth/login` | Login, returns JWT |
| `CRUD` | `/admin/api/providers` | Provider management (test connectivity via `POST /:id/test`) |
| `CRUD` | `/admin/api/models` | Model mapping management |
| `CRUD` | `/admin/api/keys` | API key management (regenerate via `POST /:id/regenerate`) |
| `GET` | `/admin/api/usage` | Usage logs with multi-dimensional filtering |
| `GET` | `/admin/api/usage/stats` | Usage statistics |
| `GET` | `/admin/api/usage/daily` | Daily usage trends |
| `GET` | `/admin/api/routing/stats` | Routing distribution: configured vs. actual weight per provider, deviation, error rate, latency, tokens, cost (see [Routing & Failover](routing-and-failover.md)) |
| `CRUD` | `/admin/api/mcp/servers` | MCP server management |
| `GET` | `/admin/api/mcp/servers/:id/tools` | List tools on an MCP server |

> The full admin surface is larger than what the dashboard exposes. Explore the route registrations in `internal/app/app.go` for the complete list.
