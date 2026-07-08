# Routing & Failover

CrossLink's routing and failover layer is what makes it production-grade rather than a round-robin proxy. This page covers the routing strategies, the circuit breaker, error classification, provider guardrails, and the routing-distribution observability API.

## Routing strategies

Six strategies, gated by license tier (`internal/router/strategy.go`):

| Strategy | Tier | Behavior |
|----------|------|----------|
| `weighted_random` | Community | Random selection weighted by configured provider weight |
| `round_robin` | Community | Even rotation across providers, coordinated across instances via Redis |
| `least_latency` | Pro | Picks the provider with the lowest rolling average latency |
| `least_busy` | Pro | Picks the provider with the fewest in-flight requests |
| `least_cost` | Enterprise | Picks the cheapest provider for the model (by token pricing) |
| `canary` | Enterprise | Routes a configured percentage of traffic to a canary provider |

### Route resolution

The resolver (`internal/router/router.go`) maps a model name to an ordered list of (provider, model) candidates:

1. Look up the model → provider mappings (cached 30s).
2. Filter out providers whose circuit breaker is `Open`.
3. Re-sort surviving candidates by `weight × healthScore` (health score cached 2s locally).
4. Fan out concurrent goroutines to fetch each candidate's average latency and active request count (for `least_latency` / `least_busy`).

Routes can carry `fallback_models` — a cross-model fallback chain used by the FallbackEngine when a provider fails.

## Circuit breaker

Each (provider, model) has a circuit breaker (`internal/provider/health.go`) with three states:

```
Closed ──errors──▶ Open ──(cooldown)──▶ HalfOpen ──probe success──▶ Closed
                                            │
                                            └──probe failure──▶ Open
```

- **Closed** — normal traffic flows.
- **Open** — all requests skip this provider; the resolver filters it out before the engine even sees it.
- **HalfOpen** — after the cooldown, a **single-flight probe** is allowed through. Only one request probes the recovered upstream (`probeInFlight` flag); the rest still skip it. This prevents a stampede when a flaky provider comes back online. On probe success → `Closed`; on failure → `Open` with a fresh cooldown.

## Error classification

The error classifier (`internal/service/error_classifier.go`) is a **DB-backed rule table**, hot-reloaded every 30s, that distinguishes two failure classes:

- **Persistent failures** (quota exhausted, billing disabled, invalid key) — *one strike and you're out*. The circuit opens immediately for a long cooldown (~30 min). Retrying is pointless; the provider won't recover without human action.
- **Transient failures** (rate limit, 5xx, network timeout) — *accumulate to a threshold*. A few transient errors don't trip the breaker; only sustained transient failures open the circuit, and for a shorter cooldown.

Each provider can configure `retry_on` (which error types to retry), `max_retries`, and `retry_delay_ms` with exponential/fixed/linear backoff. If the rule table fails to load, the last-known-good rules are retained — classification never goes down with the DB.

This is the core differentiator vs. naive retry-on-error: CrossLink doesn't waste a retry budget hammering a provider whose quota is exhausted.

## Provider guardrails

Per-(provider, model) concurrency and RPM limits (`internal/service/guardrail_dispatch.go`) — enforced via Redis Lua scripts with a **TTL heartbeat**:

- Every dispatch acquires a slot via `INCR` + `EXPIRE`.
- While a request is in flight, the TTL is refreshed (heartbeat).
- If a CrossLink process crashes mid-dispatch, the counter **self-heals** within the TTL window (~5 min) — no stuck counters blocking a provider forever.

When a provider exceeds its capacity, a `guard:limited` flag is set; the health-aware router reads it and demotes that provider's effective weight. This is deliberately a two-phase system — **count-only dispatch** + **health-score routing** — so it doesn't entangle with the FallbackEngine's error classifier.

## Routing-distribution observability

`GET /admin/api/routing/stats?model=<model>&days=7` (`internal/admin/routing.go`) returns per-provider rows:

| Field | Meaning |
|-------|---------|
| `config_weight_pct` | The configured weight percentage |
| `actual_pct` | The actual share of traffic served |
| `deviation` | Difference between configured and actual |
| `error_rate` | Per-provider error rate |
| `avg_latency_ms` | Average latency |
| `tokens` | Tokens served |
| `cost` | Cost incurred |

This "configured vs. actual" view lets you spot weight drift — e.g., a provider configured for 50% but actually serving 12% because the circuit breaker keeps opening. Combined with the `x-crosslink-fallback-*` response headers (see [API Reference](api-reference.md)), routing behavior is fully observable end to end.
