# CrossLink Documentation

This directory contains the deeper documentation for CrossLink. The [main README](../README.md) is the quick-start overview; the pages below go into the how and why.

## Pages

| Document | What it covers |
|----------|----------------|
| [Architecture](architecture.md) | Request flow, key packages, the streaming translator state machine, the fallback engine's timeout budgeting, and the 5-phase graceful shutdown. |
| [Routing & Failover](routing-and-failover.md) | The 6 routing strategies, circuit-breaker states, error classification (persistent vs. transient), self-healing provider guardrails, and routing-distribution observability. |
| [MCP Gateway](mcp-gateway.md) | Transport abstraction, per-tool RBAC, encrypted credentials, health checks, and call-log partitioning. |
| [API Reference](api-reference.md) | Full endpoint reference for the gateway, MCP, and admin APIs — including request/response shapes, error codes, and response headers. |
| [Deployment](deployment.md) | All deployment options: Docker Compose (dev/prod/CN), systemd, Nginx, Caddy, and the GM (Chinese national cryptography) variant. |

## Other resources

- [Configuration reference](../configs/) — `config.example.yaml` with inline comments
- [Contributing guide](../CONTRIBUTING.md)
- [Security policy](../SECURITY.md)

## License

CrossLink is licensed under [Apache 2.0](../LICENSE). The Community edition is fully open source; Pro and Enterprise tiers add commercial features on top of the same core.
