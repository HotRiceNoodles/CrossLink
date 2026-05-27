<div align="center">

<img src="imgs/CrossLinkBanner_zh.png" alt="CrossLink 横幅">

<br/>

<img src="https://img.shields.io/badge/Go-1.22+-00ADD8?style=for-the-badge&logo=go" alt="Go Version">
<img src="https://img.shields.io/badge/License-Apache%202.0-blue?style=for-the-badge" alt="License">
<img src="https://img.shields.io/badge/PRs-Welcome-brightgreen?style=for-the-badge" alt="PRs Welcome">

<br/>
<br/>

# CrossLink 搭桥

### 一个网关，调度全球最强模型

**OpenAI & Anthropic 兼容 LLM API 网关**

[English](README.md) | [中文](#快速开始)

智能路由、自动容灾、协议翻译、限流缓存、MCP 网关、可视化管理后台——开箱即用。

[快速开始](#快速开始) · [功能特性](#功能特性) · [架构](#架构) · [API 文档](#api-端点) · [参与贡献](CONTRIBUTING.md)

</div>

---

## 为什么选择 CrossLink？

每家 LLM 供应商的 API 格式、认证方式、功能特性各不相同。
每接入一个新模型，就要重写一遍适配代码——既繁琐又容易出错，还让你被厂商牢牢锁死。

CrossLink 充当你应用与任意 LLM 供应商之间的**万能转接头**：

- **统一入口** — 你的代码只需对接一个 API，OpenAI 或 Anthropic 格式任选
- **任意供应商** — 请求被智能路由到 OpenAI、Anthropic、Azure、DeepSeek、通义千问、Ollama 或任何 OpenAI 兼容服务
- **自动翻译** — 完整的双向协议转换，覆盖流式 SSE、Tool Use、Extended Thinking
- **天然容灾** — 熔断器、故障转移链、重试策略，供应商挂了你的应用不会挂

---

## 功能特性

### 核心网关

- **双协议** — 同时暴露 `/v1/chat/completions`（OpenAI）和 `/v1/messages`（Anthropic）端点，自动双向协议转换
- **多供应商** — 路由到 OpenAI、Anthropic、Azure OpenAI、DeepSeek、通义千问、Moonshot、Ollama 及任意 OpenAI 兼容供应商
- **智能路由** — 6 种策略：加权随机、轮询、最低延迟、最低成本、最空闲、金丝雀发布
- **自动容灾** — 多供应商故障转移链，配合熔断器、可配置重试策略（指数/固定/线性退避）和错误分类
- **响应缓存** — 基于 Redis 的缓存，按模型设置 TTL、gzip 压缩、按用户隔离缓存键

### 安全与管控

- **限流** — 按 API Key 设置 RPM/TPM 限制，全局并发控制（2000）
- **RBAC 权限** — 基于角色的访问控制，覆盖供应商、模型、API Key 和 MCP
- **预算管理** — 按 Key 和按团队的预算上限，超支自动熔断
- **内容防护** — 可配置的内容安全引擎框架，支持自定义规则和动作
- **国密支持** — 支持国际标准（SHA-256/RSA/AES）和国密算法（SM3/SM2/SM4）

### 可观测性

- **用量分析** — 每次请求自动记录 Token 用量、成本、延迟、缓存命中率和故障转移/重试次数
- **Prometheus 指标** — 内置 metrics 端点
- **OpenTelemetry** — 分布式链路追踪
- **结构化日志** — 带请求上下文的 JSON 日志

### MCP 网关

- **Model Context Protocol** — HTTP/SSE 传输、工具发现与缓存、健康检查
- **权限管理** — 按主体（Key / 团队 / 角色）的工具访问控制
- **调用日志** — 完整的工具调用日志，按月分区，自动清理

### 运维管理

- **Vue 3 管理后台** — 内置 Web 管理界面，供应商、模型、Key、用量、MCP 一站式管理（[CrossLink-UI-Standard](https://github.com/HotRiceNoodles/CrossLink-UI-Standard)）
- **多实例部署** — Redis Pub/Sub 供应商注册同步、分布式轮询、密钥轮换
- **优雅关闭** — 4 阶段排空：SSE 流排空 → HTTP 关闭 → Worker 刷盘 → 后台协程取消
- **一键部署** — Docker Compose 开发/生产环境，包含 Caddy 自动 TLS

---

## 架构

<p align="center">
  <img src="imgs/Architecture_zh.png" alt="CrossLink 架构" width="720">
</p>

---

## 快速开始

### 前置要求

- Go 1.22+（源码编译）
- PostgreSQL 14+
- Redis 7+

### Docker Compose（推荐）

```bash
git clone https://github.com/HotRiceNoodles/CrossLink.git
cd CrossLink/deployments
docker compose -f docker-compose.dev.yaml up
```

网关启动于 `http://localhost:8080`，管理后台即可使用。

### 源码编译

```bash
git clone https://github.com/HotRiceNoodles/CrossLink.git
cd CrossLink
cp configs/config.example.yaml configs/config.yaml
# 编辑 config.yaml，填入数据库和 Redis 连接信息
make build
./bin/crosslink
```

### 发起第一次请求

通过管理后台（`http://localhost:8080`）创建 API Key，然后：

**OpenAI SDK（Python）**

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="cl-your-api-key"
)

response = client.chat.completions.create(
    model="deepseek-chat",
    messages=[{"role": "user", "content": "你好！"}]
)
print(response.choices[0].message.content)
```

**Anthropic SDK（Python）**

```python
import anthropic

client = anthropic.Anthropic(
    base_url="http://localhost:8080",
    api_key="cl-your-api-key"
)

message = client.messages.create(
    model="claude-sonnet-4-20250514",
    max_tokens=1024,
    messages=[{"role": "user", "content": "你好！"}]
)
print(message.content[0].text)
```

**curl**

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer cl-your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-chat",
    "messages": [{"role": "user", "content": "你好！"}]
  }'
```

---

## 配置

所有配置位于 `configs/config.yaml`，每项均可通过 `CL_` 前缀的环境变量覆盖（如 `CL_DATABASE_HOST`、`CL_REDIS_PORT`）。

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

cache:
  enabled: true
  default_ttl: 5m

mcp:
  enabled: true
  health_check_interval: 30s

crypto:
  mode: standard    # standard (SHA-256/RSA/AES) 或 gm (SM3/SM2/SM4)
```

### 供应商初始化

首次启动时从 `configs/providers.yaml` 加载供应商配置：

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

---

## API 端点

### 网关接口（需 API Key）

| 方法 | 路径 | 说明 |
|--------|------|-------------|
| `POST` | `/v1/chat/completions` | OpenAI 兼容对话（流式 & 非流式） |
| `POST` | `/v1/messages` | Anthropic 兼容消息（流式 & 非流式） |
| `GET` | `/v1/models` | 获取可用模型列表 |

### MCP 网关

| 方法 | 路径 | 说明 |
|--------|------|-------------|
| `POST` | `/mcp/:server` | MCP JSON-RPC 转发 |
| `GET` | `/mcp/:server` | MCP SSE 传输 |

### 管理接口（需 JWT）

| 方法 | 路径 | 说明 |
|--------|------|-------------|
| `POST` | `/admin/api/auth/login` | 登录 |
| `CRUD` | `/admin/api/providers` | 供应商管理（通过 `POST /:id/test` 测试连通性） |
| `CRUD` | `/admin/api/models` | 模型映射管理 |
| `CRUD` | `/admin/api/keys` | API Key 管理（通过 `POST /:id/regenerate` 重新生成） |
| `GET` | `/admin/api/usage` | 用量日志，支持多维度筛选 |
| `GET` | `/admin/api/usage/stats` | 用量统计 |
| `GET` | `/admin/api/usage/daily` | 每日趋势 |
| `CRUD` | `/admin/api/mcp/servers` | MCP 服务管理 |
| `GET` | `/admin/api/mcp/servers/:id/tools` | MCP 服务工具列表 |

完整 API 文档请参阅 [文档](docs/)。

---

## 部署

### 生产环境 Docker Compose

```bash
cd deployments
docker compose -f docker-compose.prod.yaml up -d
```

包含 Caddy 反向代理，通过 Let's Encrypt 自动获取 TLS 证书。

### Nginx

生产就绪的 Nginx 配置（TLS、安全头、SSE 流式支持）：`deployments/nginx/crosslink.conf`

### Systemd

```bash
sudo cp deployments/systemd/crosslink.service /etc/systemd/system/
sudo systemctl enable --now crosslink
```

### 国密部署

符合国密标准（SM2/SM3/SM4）的专用部署方案，包含 GmSSL/Nginx 和 TLCP 协议支持：`deployments/gm/`

---

## 参与贡献

欢迎所有形式的贡献——Bug 修复、新功能、文档完善、想法建议。

1. Fork 本仓库
2. 创建特性分支（`git checkout -b feature/my-feature`）
3. 提交变更（`git commit -m 'Add some feature'`）
4. 推送到分支（`git push origin feature/my-feature`）
5. 发起 Pull Request

详细指南请参阅 [CONTRIBUTING.md](CONTRIBUTING.md)。

### 开发

```bash
make build          # 编译（输出 bin/crosslink）
make run            # 运行
make test           # 运行测试
make lint           # 代码检查
make clean          # 清理构建产物
```

---

## 安全

请参阅 [SECURITY.md](SECURITY.md) 了解安全政策和漏洞报告方式。

---

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=HotRiceNoodles/CrossLink&type=Date)](https://star-history.com/#HotRiceNoodles/CrossLink&Date)

---

## 许可证

[Apache License 2.0](LICENSE)