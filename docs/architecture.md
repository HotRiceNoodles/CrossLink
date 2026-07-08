# Architecture

CrossLink is a multi-tenant LLM API gateway written in Go. The central design choice: **every request is normalized to OpenAI format internally**, routed to a provider (which speaks OpenAI protocol), then translated back to whatever format the client asked for. One internal protocol, many external shapes.

## Request flow

```
Client → Auth/RBAC/RateLimit middleware → Route Resolver (model→provider mapping)
       → FallbackEngine → Provider Adapter → Upstream LLM
       → translate response back → Client
```

Gateway endpoints:

- `/v1/chat/completions` — OpenAI protocol
- `/v1/messages` — Anthropic protocol
- `/v1/models`
- `/mcp/:server` — MCP gateway

## Middleware chain

Applied in order (see `internal/app/app.go:FullSetup`):

```
Drain → ConcurrencyLimit(2000) → ReadBody(10MB) → Debug → UsageLog
      → AuthFailureLimit → Auth → RequireModel → Guardrails → Cache
      → RateLimit → TPMLimit → BudgetCheck → ReportTokens → ReportBudgetUsage
```

The `Drain` middleware is what enables graceful shutdown: during drain, new requests get an immediate `503` while in-flight requests finish.

## Key packages

| Package | Responsibility |
|---------|----------------|
| `internal/app/` | Application bootstrap. `app.go:FullSetup()` wires all dependencies, registers middleware and routes, starts workers, handles shutdown. |
| `internal/app/interfaces.go` | `Extensions` struct — plugin hooks for the Community/Commercial split. Community uses `NoopGate`; commercial injects extra routes, strategies, middlewares. |
| `internal/domain/` | API request/response DTOs shared across all layers (OpenAI, Anthropic, embeddings, image, audio, batch). |
| `internal/model/` | GORM database models (Provider, ProviderModel, APIKey, User, Team, Role, …). |
| `internal/provider/` | Provider abstraction with the adapter pattern. `Provider` interface (Chat/StreamChat). Adapters register via `RegisterAdapter(adapterType, factory, meta)`. Built-in: `openai_compatible`, `anthropic`, `azure_openai`. |
| `internal/router/` | Route resolver: maps model names to provider instances, applies routing strategies. |
| `internal/service/gateway.go` | GatewayService — the Anthropic protocol path: translate → resolve route → execute via FallbackEngine → translate back. |
| `internal/service/fallback_engine.go` | Multi-provider failover with circuit-breaker awareness and per-provider timeout allocation. |
| `internal/translator/` | Anthropic↔OpenAI translation for streaming and non-streaming. `StreamTranslator` is a state machine converting OpenAI SSE chunks to Anthropic SSE events. |
| `internal/middleware/` | The Gin middleware chain. |
| `internal/config/` | Viper-based configuration, `CL_` env prefix. |
| `internal/crypto/` | Crypto abstraction: standard (SHA-256/AES/RSA) and GM (SM3/SM4/SM2) modes. |
| `internal/secret/` | Multi-source secret resolution (env vars, encrypted DB store) with background key-rotation watcher. |
| `internal/mcp/` | MCP gateway: server registry, HTTP/SSE transport, tool discovery, permission management, health checks. |

## The streaming translator state machine

The translator (`internal/translator/stream.go`) is a 4-state finite state machine that converts OpenAI SSE chunks into Anthropic SSE events in real time:

```
stateInit → stateStarted → stateBlockActive → stateDone
```

It handles three block types — `thinking`, `text`, and `tool_use` — and the transitions between them. Notable details:

- **Partial JSON for tool arguments** — tool-call arguments arrive incrementally across chunks; the FSM accumulates and emits them as Anthropic `input_json_delta` events.
- **Extended thinking** — OpenAI reasoning content is translated to Anthropic `thinking` blocks (and vice versa).
- **Token counting** — usage is reconciled at stream end even when the upstream reports it piecemeal.

The reverse direction (Anthropic → OpenAI) lives in `internal/translator/reverse_stream.go`. A third path, OpenAI Responses API ↔ Chat, lives in `internal/translator/responses.go` and `responses_stream.go`. All three directions are full streaming state machines — not request-level rewrites.

## Fallback engine

`FallbackEngine` (`internal/service/fallback_engine.go`) executes a route — a chain of (provider, model) candidates — with:

- **Per-provider timeout budgeting.** The remaining deadline is divided across the remaining attempts, clamped to a 5s–30s window per provider. A slow upstream on attempt 1 doesn't eat the budget for attempt 2.
- **Error classification** (see [Routing & Failover](routing-and-failover.md)) — retryable vs. non-retryable, persistent vs. transient.
- **Cross-model fallback.** A route can carry `fallback_models`: if provider A fails, the engine can retry on a different model on provider B.
- **Dedup.** Candidates are deduplicated so the same provider+model isn't attempted twice in one chain.

## Graceful shutdown (5 phases)

1. **Drain in-flight SSE streams** (60s) — `DrainingManager` flips an atomic flag; new requests get `503` immediately, in-flight requests finish via a `WaitGroup`.
2. **HTTP server shutdown** (10s).
3. **Flush usage workers** (10s) — the bounded worker pool drains its queued async tasks (usage logging, etc.).
4. **Cancel background goroutines** — health checks, secret-rotation watchers, route-cache refreshers.
5. **DB cleanup** — release the migration advisory lock, close connections.

The HTTP `WriteTimeout` is intentionally set to `0` so streaming responses are never killed mid-stream by the server's own timeout.

## Concurrency model

- **Global cap of 2000** concurrent gateway requests, enforced by a buffered-channel semaphore.
- **`sync.Pool` for SSE parsing buffers** — reusable 64KB scanner and byte buffers prevent GC pressure on high-throughput streaming.
- **Bounded worker pool** with per-task panic recovery for async work (usage logging, MCP call logging).
- **Per-(provider, model) health scores** cached locally for 2s to bound Redis reads on the hot path; route resolution caches for 30s and re-sorts candidates by `weight × healthScore` on cache hit.
