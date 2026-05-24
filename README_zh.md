<div align="center">

# CrossLink

**OpenAI & Anthropic 兼容 API 网关 — 社区版**

统一大模型代理网关，提供负载均衡、故障转移、限流、缓存及 MCP 网关能力。

[English](README.md) | [中文](#功能特性)

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go)](https://go.dev/)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](CONTRIBUTING.md)

</div>

---

## 功能特性

- **多供应商网关** — 支持 OpenAI、Anthropic、Azure OpenAI、DeepSeek、通义千问、Moonshot、Ollama 及任意 OpenAI 兼容供应商
- **双 API 格式** — 同时暴露 `/v1/chat/completions`（OpenAI）和 `/v1/messages`（Anthropic）端点，自动协议转换
- **负载均衡** — 加权随机、轮询策略
- **自动故障转移与重试** — 多供应商故障转移链，支持可配置的重试策略（指数/固定/线性退避）
- **熔断器** — 按供应商健康追踪，可配置失败阈值
- **响应缓存** — 基于 Redis 的缓存，支持按模型设置 TTL 和 gzip 压缩
- **限流** — 按 API Key 设置 RPM/TPM 限制，全局并发控制（2000）
- **RBAC 权限** — 基于角色的访问控制，覆盖供应商、模型、API Key、用量分析和 MCP 管理
- **MCP 网关** — Model Context Protocol 代理，支持 HTTP/SSE 传输、工具发现、权限管理和健康检查
- **用量分析** — 完整的请求日志，含成本追踪、缓存命中和故障转移/重试指标
- **可观测性** — Prometheus 指标、OpenTelemetry 链路追踪、结构化 JSON 日志
- **国密支持** — 支持国际标准（SHA-256/RSA/AES）和国密算法（SM3/SM2/SM4）
- **多实例部署** — 通过 Redis Pub/Sub 同步供应商注册、分布式轮询和密钥轮换
- **优雅关闭** — 多阶段排空处理进行中的 SSE 流
- **Vue 3 管理后台** — 内置 Web 管理界面

## 架构

```
                    ┌──────────────┐
                    │    客户端     │
                    └──────┬───────┘
                           │
                    ┌──────▼───────┐
                    │  认证 / RBAC │
                    │    限流       │
                    └──────┬───────┘
                           │
                    ┌──────▼───────┐
                    │    路由器     │──── 模型 → 供应商映射
                    │    解析器     │──── 策略选择
                    └──────┬───────┘
                           │
              ┌────────────┼────────────┐
              │            │            │
        ┌─────▼─────┐┌────▼────┐┌─────▼─────┐
        │  OpenAI   ││Anthropic││  Azure    │
        │  兼容适配器 ││  适配器  ││  OpenAI   │
        │           ││         ││  适配器    │
        └─────┬─────┘└────┬────┘└─────┬─────┘
              │            │            │
        ┌─────┴─────┐     │            │
        │  Ollama   │     │            │
        │  适配器    │     │            │
        └─────┬─────┘     │            │
              │            │            │
              └────────────┼────────────┘
                           │
                    ┌──────▼───────┐
                    │  PostgreSQL  │  Redis  │
                    │  (配置,      │  (缓存, │
                    │   用量,      │   限流, │
                    │   RBAC)      │   同步) │
                    └──────────────────────┘
```

## 快速开始

### 前置要求

- Go 1.22+
- PostgreSQL 14+
- Redis 7+

### 方式一：Docker Compose（推荐）

```bash
cd deployments
docker compose -f docker-compose.dev.yaml up
```

自动启动 PostgreSQL、Redis 和网关服务。管理后台访问地址：`http://localhost:8080`。

### 方式二：源码编译

```bash
# 克隆仓库
git clone https://github.com/crosslink/gateway.git
cd gateway

# 复制并编辑配置文件
cp configs/config.example.yaml configs/config.yaml

# 编译运行
make build
./bin/crosslink
```

### 方式三：Go Run

```bash
cp configs/config.example.yaml configs/config.yaml
# 编辑 config.yaml，填入数据库和 Redis 连接信息
go run ./cmd/server
```

## 配置说明

配置文件位于 `configs/config.yaml`，所有配置项均可通过 `CL_` 前缀的环境变量覆盖。

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
  mode: standard    # standard (SHA-256/RSA/AES) 或 gm (SM3/SM2/SM4)
```

### 供应商初始化

首次启动时，供应商配置从 `configs/providers.yaml` 加载。支持环境变量插值：

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

## API 端点

### 网关接口（需 API Key）

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/chat/completions` | OpenAI 兼容对话补全（流式 & 非流式） |
| POST | `/v1/messages` | Anthropic 兼容消息接口（流式 & 非流式） |
| GET | `/v1/models` | 获取可用模型列表 |

### MCP 网关

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/mcp/:server` | MCP JSON-RPC 转发 |
| GET | `/mcp/:server` | MCP SSE 传输 |

### 管理接口（需 JWT）

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/admin/api/auth/login` | 登录 |
| POST | `/admin/api/auth/logout` | 登出 |
| GET | `/admin/api/version` | 版本信息 |
| GET | `/admin/api/adapters` | 适配器类型列表 |
| CRUD | `/admin/api/providers` | 供应商管理（通过 `POST /:id/test` 测试连通性） |
| CRUD | `/admin/api/models` | 模型映射管理 |
| CRUD | `/admin/api/keys` | API Key 管理（通过 `POST /:id/regenerate` 重新生成） |
| GET | `/admin/api/usage` | 用量日志 |
| GET | `/admin/api/usage/stats` | 用量统计 |
| GET | `/admin/api/usage/daily` | 每日趋势 |
| GET | `/admin/api/usage/models` | 模型分布 |
| POST | `/admin/api/system/password` | 修改管理员密码 |
| GET | `/admin/api/auth/permissions` | 当前用户权限 |
| GET/PUT | `/admin/api/user/preferences` | 用户偏好设置 |

### MCP 管理（需 JWT）

| 方法 | 路径 | 说明 |
|------|------|------|
| CRUD | `/admin/api/mcp/servers` | MCP 服务管理（通过 `POST /:id/test` 测试连通性） |
| GET | `/admin/api/mcp/servers/:id/tools` | MCP 服务工具列表 |
| GET/POST/DELETE | `/admin/api/mcp/servers/:id/permissions[/:pid]` | 工具权限管理 |
| GET | `/admin/api/mcp/servers/:id/logs` | MCP 服务日志 |
| GET | `/admin/api/mcp/servers/:id/stats` | MCP 服务统计 |
| GET | `/admin/api/mcp/stats` | 全局 MCP 统计 |

### 系统接口

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/health` | 健康检查（无需认证） |
| GET | `/metrics` | Prometheus 指标 |

## 使用示例

启动网关后，将 OpenAI SDK 指向网关：

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="your-api-key"  # 通过管理后台创建
)

response = client.chat.completions.create(
    model="gpt-4o",
    messages=[{"role": "user", "content": "你好！"}]
)
print(response.choices[0].message.content)
```

或使用 Anthropic Messages API：

```python
import anthropic

client = anthropic.Anthropic(
    base_url="http://localhost:8080",
    api_key="your-api-key"
)

message = client.messages.create(
    model="claude-sonnet-4-20250514",
    max_tokens=1024,
    messages=[{"role": "user", "content": "你好！"}]
)
print(message.content[0].text)
```

## 部署

### 生产环境 Docker Compose

```bash
cd deployments
docker compose -f docker-compose.prod.yaml up -d
```

包含 Caddy 反向代理，支持通过 Let's Encrypt 自动获取 TLS 证书。

### Nginx

生产就绪的 Nginx 配置位于 `deployments/nginx/crosslink.conf`，包含 TLS、安全头和 SSE 流式支持。

### Systemd

```bash
sudo cp deployments/systemd/crosslink.service /etc/systemd/system/
sudo systemctl enable crosslink
sudo systemctl start crosslink
```

### 国密部署

提供符合国密标准（SM2/SM3/SM4）的专用部署方案，位于 `deployments/gm/`，包含 GmSSL/Nginx 和 TLCP 协议支持。

## 开发

```bash
make build          # 编译（输出 bin/crosslink）
make run            # 运行
make test           # 运行测试
make lint           # 代码检查
make clean          # 清理构建产物
make release        # 构建发布版本
```

贡献指南请参阅 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 安全

请参阅 [SECURITY.md](SECURITY.md) 了解安全政策和漏洞报告方式。

## 许可证

本项目基于 [Apache License 2.0](LICENSE) 许可。
