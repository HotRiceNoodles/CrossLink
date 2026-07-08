# Deployment

CrossLink ships with several deployment variants. Pick the one that matches your environment; the application binary is the same in all of them — only the orchestration differs.

## Prerequisites

- Go 1.22+ (only if building from source)
- PostgreSQL 14+
- Redis 7+

## Docker Compose

### Dev — gateway + frontend + PostgreSQL + Redis in one command

```bash
git clone https://github.com/HotRiceNoodles/CrossLink.git
cd CrossLink
docker compose -f deployments/docker-compose.dev.yaml up --build
```

Frontend dashboard and API gateway are available at `http://localhost` (port 80).

### Prod

```bash
docker compose -f deployments/docker-compose.prod.yaml up -d --build
```

The prod compose file is tuned for production: resource limits, restart policies, and separated networks.

### China network

Use the CN variant with Go proxy (`goproxy.cn`) and npm mirror (`registry.npmmirror.com`) pre-configured — avoids Docker build failures on slow international pulls:

```bash
docker compose -f deployments/docker-compose.cn.yaml up --build
```

## Build from source

```bash
git clone https://github.com/HotRiceNoodles/CrossLink.git
cd CrossLink
cp configs/config.example.yaml configs/config.yaml
# Edit config.yaml with your database and Redis settings
make build
./bin/crosslink
```

Build with a pinned version:

```bash
go build -ldflags "-X github.com/crosslink/internal/version.Version=v1.0.0" \
  -o bin/crosslink ./cmd/server
```

## Reverse proxy

### Nginx

Production-ready config with TLS, security headers, and SSE streaming support:

```
deployments/nginx/crosslink.conf       # backend + frontend
deployments/nginx/frontend-proxy.conf  # frontend only
```

The SSE config is important: Nginx must disable response buffering (`proxy_buffering off;`) and set a long read timeout, or streaming responses will stall.

### Caddy

A minimal `Caddyfile` (`deployments/Caddyfile`) for automatic HTTPS via Let's Encrypt.

## Systemd

For bare-metal / VM deployment:

```bash
sudo cp deployments/systemd/crosslink.service /etc/systemd/system/
sudo systemctl enable --now crosslink
```

## GM (Chinese national cryptography) deployment

For SM2/SM3/SM4 compliance — required for Chinese government and financial-sector deployments — a dedicated variant is provided:

```
deployments/gm/docker-compose.yml   # GM-aware compose
deployments/gm/nginx-gm.conf        # GmSSL/Nginx with TLCP protocol
```

Set `crypto.mode: gm` in `configs/config.yaml` to switch the application to SM3/SM2/SM4 (including HMAC-SM3 JWT signing for admin sessions). Combined with the self-hosted slider CAPTCHA (no reCAPTCHA/hCaptcha dependency), this enables fully air-gapped,信创-compliant deployments.

## Configuration

All runtime config lives in `configs/config.yaml`. Every value is overridable with the `CL_` env prefix (e.g., `CL_DATABASE_HOST`, `CL_REDIS_PORT`). See `configs/config.example.yaml` for the full list with inline comments.

Providers are seeded from `configs/providers.yaml` on first run — see the [main README](../README.md#configuration) for an example.

## Multi-instance

For horizontal scaling, run multiple CrossLink instances behind a load balancer. State is shared via Redis:

- **Provider registry sync** — Redis Pub/Sub propagates provider config changes across instances.
- **Distributed round-robin** — the `round_robin` strategy coordinates across instances so traffic is evenly distributed, not doubled per instance.
- **Shared circuit-breaker state** — health scores and `guard:limited` flags live in Redis, so one instance opening a circuit is visible to all.

Use a PostgreSQL primary with the advisory lock (`pg_advisory_lock(20260518)`) to prevent concurrent migration runs during rolling deploys.
