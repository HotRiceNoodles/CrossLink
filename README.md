<div align="center">

# CrossLink

**OpenAI & Anthropic Compatible API Gateway — Community Edition**

Unified LLM proxy with load balancing, fallback, rate limiting, caching, and MCP gateway.

[English](#features) | [中文文档](README_zh.md)

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go)](https://go.dev/)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](CONTRIBUTING.md)

</div>

---

## Features

- **Multi-Provider Gateway** — Proxy requests to OpenAI, Anthropic, Azure OpenAI, DeepSeek, Qwen, Moonshot, Ollama, and any OpenAI-compatible provider
- **Dual API Format** — Exposes both `/v1/chat/completions` (OpenAI) and `/v1/messages` (Anthropic) endpoints with automatic protocol translation
- **Load Balancing** — Weighted random and round-robin strategies
- **Automatic Fallback & Retry** — Multi-provider fallback chains with configurable retry policies (exponential/fixed/linear backoff)
- **Circuit Breaker** — Per-provider health tracking with configurable failure thresholds
- **Response Caching** — Redis-based caching with per-model TTL and gzip compression
- **Rate Limiting** — Per-key RPM/TPM limits with global concurrency control (2000)
- **RBAC** — Role-based access control for providers, models, API keys, usage analytics, and MCP
- **MCP Gateway** — Model Context Protocol proxy with HTTP/SSE transport, tool discovery, permission management, and health checks
- **Usage Analytics** — Comprehensive logging with cost tracking, cache hits, and fallback/retry metrics
- **Observability** — Prometheus metrics, OpenTelemetry tracing, structured JSON logging
- **Crypto Flexibility** — Standard (SHA-256/RSA/AES) or Chinese national cryptography (SM3/SM2/SM4)
- **Multi-Instance** — Provider registry sync via Redis Pub/Sub, distributed round-robin, and encryption key rotation
- **Graceful Shutdown** — Multi-phase drain for in-flight SSE streams
- **Vue 3 Admin Dashboard** — Built-in web UI for management and monitoring ([CrossLink-UI-Standard](https://github.com/HotRiceNoodles/CrossLink-UI-Standard))

## Architecture

```
                    ┌──────────────┐
                    │   Client     │
                    └──────┬───────┘
                           │
                    ┌──────▼───────┐
                    │  Auth / RBAC │
                    │  Rate Limit  │
                    └──────┬───────┘
                           │
                    ┌──────▼───────┐
                    │   Router     │──── Model → Provider Mapping
                    │   Resolver   │──── Strategy Selection
                    └──────┬───────┘
                           │
              ┌────────────┼────────────┐
              │            │            │
        ┌─────▼─────┐┌────▼────┐┌─────▼─────┐
        │  OpenAI   ││Anthropic││  Azure    │
        │ Compatible││ Adapter ││  OpenAI   │
        │  Adapter  ││         ││  Adapter  │
        └─────┬─────┘└────┬────┘└─────┬─────┘
              │            │            │
        ┌─────┴─────┐     │            │
        │  Ollama   │     │            │
        │  Adapter  │     │            │
        └─────┬─────┘     │            │
              │            │            │
              └────────────┼────────────┘
                           │
                    ┌──────▼───────┐
                    │  PostgreSQL  │  Redis  │
                    │  (Config,    │  (Cache,│
                    │   Usage,     │   Rate  │
                    │   RBAC)      │   Sync) │
                    └──────────────────────┘
```

## Quick Start

### Prerequisites

- Go 1.22+
- PostgreSQL 14+
- Redis 7+

### Option 1: Docker Compose (Recommended)

```bash
cd deployments
docker compose -f docker-compose.dev.yaml up
```

This starts PostgreSQL, Redis, and the gateway. The admin dashboard is available at `http://localhost:8080`.

### Option 2: Build from Source

```bash
# Clone the repository
git clone https://github.com/crosslink/gateway.git
cd gateway

# Copy and edit configuration
cp configs/config.example.yaml configs/config.yaml

# Build and run
make build
./bin/crosslink
```

### Option 3: Go Run

```bash
cp configs/config.example.yaml configs/config.yaml
# Edit config.yaml with your database and Redis settings
go run ./cmd/server
```

## Configuration

Configuration is managed via `configs/config.yaml`. All values can be overridden with environment variables using the `CL_` prefix.

```yaml
server:
  port: 8080
  read_timeout: 30s
  write_timeout: 120s

database:
  host: localhost
  port: 5432
  user: crosslink
  password: crosslink
  dbname: crosslink
  sslmode: disable

redis:
  host: localhost
  port: 6379

gateway:
  auth_key: "cl-change-me"

admin:
  username: admin
  password: changeme
  jwt_secret: "change-me-to-a-random-secret"
  token_expiry: 24

rate_limit:
  rpm: 60
  tpm: 100000

logging:
  level: info
  format: json

cache:
  enabled: true
  default_ttl: 5m
  embeddings_ttl: 60m
  max_body_size: 10485760

mcp:
  enabled: true
  max_servers: 0
  health_check_interval: 30s
  tool_cache_ttl: 5m
  rate_limit_enabled: true
  rate_limit_default_rpm: 60

crypto:
  mode: standard    # standard (SHA-256/RSA/AES) or gm (SM3/SM2/SM4)
```

### Provider Seeding

On first run, providers are seeded from `configs/providers.yaml`. You can configure multiple providers with environment variable interpolation:

```yaml
providers:
  - name: deepseek
    adapter_type: openai_compatible
    base_url: https://api.deepseek.com/v1
    api_key: ${DEEPSEEK_API_KEY}
    models:
      - name: deepseek-chat
        provider_model: deepseek-chat
```

## API Endpoints

### Gateway (API Key Required)

| Method | Path | Description |
|--------|------|-------------|
| POST | `/v1/chat/completions` | OpenAI-compatible chat completions (stream & non-stream) |
| POST | `/v1/messages` | Anthropic-compatible messages (stream & non-stream) |
| GET | `/v1/models` | List available models |

### MCP Gateway

| Method | Path | Description |
|--------|------|-------------|
| POST | `/mcp/:server` | MCP JSON-RPC forwarding |
| GET | `/mcp/:server` | MCP SSE transport |

### Admin (JWT Required)

| Method | Path | Description |
|--------|------|-------------|
| POST | `/admin/api/auth/login` | Login |
| POST | `/admin/api/auth/logout` | Logout |
| GET | `/admin/api/version` | Version info |
| GET | `/admin/api/adapters` | List adapter types |
| CRUD | `/admin/api/providers` | Provider management (test via `POST /:id/test`) |
| CRUD | `/admin/api/models` | Model mapping management |
| CRUD | `/admin/api/keys` | API key management (regenerate via `POST /:id/regenerate`) |
| GET | `/admin/api/usage` | Usage logs |
| GET | `/admin/api/usage/stats` | Usage statistics |
| GET | `/admin/api/usage/daily` | Daily usage trends |
| GET | `/admin/api/usage/models` | Model distribution |
| POST | `/admin/api/system/password` | Change admin password |
| GET | `/admin/api/auth/permissions` | Current user permissions |
| GET/PUT | `/admin/api/user/preferences` | User preferences |

### MCP Admin (JWT Required)

| Method | Path | Description |
|--------|------|-------------|
| CRUD | `/admin/api/mcp/servers` | MCP server management (test via `POST /:id/test`) |
| GET | `/admin/api/mcp/servers/:id/tools` | List tools on MCP server |
| GET/POST/DELETE | `/admin/api/mcp/servers/:id/permissions[/:pid]` | Tool permission management |
| GET | `/admin/api/mcp/servers/:id/logs` | MCP server logs |
| GET | `/admin/api/mcp/servers/:id/stats` | MCP server stats |
| GET | `/admin/api/mcp/stats` | Global MCP stats |

### System

| Method | Path | Description |
|--------|------|-------------|
| GET | `/health` | Health check (no auth) |
| GET | `/metrics` | Prometheus metrics (gateway auth key) |

## Usage Example

After starting the gateway, point your OpenAI SDK to it:

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="your-api-key"  # Created via admin dashboard
)

response = client.chat.completions.create(
    model="gpt-4o",
    messages=[{"role": "user", "content": "Hello!"}]
)
print(response.choices[0].message.content)
```

Or use the Anthropic Messages API:

```python
import anthropic

client = anthropic.Anthropic(
    base_url="http://localhost:8080",
    api_key="your-api-key"
)

message = client.messages.create(
    model="claude-sonnet-4-20250514",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Hello!"}]
)
print(message.content[0].text)
```

## Deployment

### Production Docker Compose

```bash
cd deployments
docker compose -f docker-compose.prod.yaml up -d
```

Includes Caddy reverse proxy with automatic TLS via Let's Encrypt.

### Nginx

A production-ready Nginx configuration is provided at `deployments/nginx/crosslink.conf` with TLS, security headers, and SSE streaming support.

### Systemd

```bash
sudo cp deployments/systemd/crosslink.service /etc/systemd/system/
sudo systemctl enable crosslink
sudo systemctl start crosslink
```

### GM (Chinese National Cryptography)

For compliance with GM standards (SM2/SM3/SM4), a dedicated deployment is provided at `deployments/gm/` with GmSSL/Nginx and TLCP protocol support.

## Development

```bash
make build          # Build binary (bin/crosslink)
make run            # Run the server
make test           # Run all tests
make lint           # Run golangci-lint
make clean          # Remove build artifacts
make release        # Build release binary with version info
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for contribution guidelines.

## Security

See [SECURITY.md](SECURITY.md) for our security policy and vulnerability reporting instructions.

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=HotRiceNoodles/CrossLink&type=Date)](https://star-history.com/#HotRiceNoodles/CrossLink&Date)

## License

This project is licensed under the [Apache License 2.0](LICENSE).
