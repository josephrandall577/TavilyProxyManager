# Tavily 代理池 & 管理面板

简体中文 | [English](./README_EN.md)

一个透明的 Tavily API 反向代理：将多个 Tavily API Key（额度/credits）汇聚在一个 **Master Key** 之后，并提供内置 Web UI 用于管理 Key、用量与请求日志。

---

## 🚀 功能特性

- **透明代理**：完整转发至 `https://api.tavily.com`（支持所有路径与方法）。
- **Master Key 鉴权**：客户端通过 `Authorization: Bearer <MasterKey>` 安全访问。
- **智能 Key 池管理**：
  - 优先使用剩余额度最高的 Key。
  - 同额度 Key 随机打散，有效防止请求过于集中触发频率限制。
- **自动故障切换**：遇到 `401` / `429` / `432` / `433` 等错误时，自动尝试 Key 池中的下一个可用 Key。
- **MCP 支持**：内置 HTTP MCP (Model Context Protocol) 端点，可轻松接入 Claude、VS Code 等 AI 工具。
- **可视化管理面板**：
  - **Key 管理**：便捷添加、删除及同步多个 Tavily Key 的额度信息。
  - **用量统计**：通过图表直观展示请求量与额度消耗趋势。
  - **请求日志**：详细记录每次请求，支持过滤筛选与手动清理。
- **自动化任务**：每月 1 号自动重置额度，定期清理历史日志。
- **开箱即用**：Go 二进制单文件部署，内嵌 Web UI（Vite + Vue 3 + Naive UI）。

---

## 🛠️ 环境要求

- **Docker / Docker Compose** (推荐部署方式，无需本地环境)
- **Go**: `1.23+` & **Node.js**: `20+` (仅用于本地手动编译)

---

## 📦 快速部署 (Docker)

直接使用 GHCR 镜像部署，**无需本地编译**。

### 1. 使用 Docker Compose (推荐)

创建 `docker-compose.yml` 文件：

```yaml
version: "3.8"
services:
  tavily-proxy:
    image: ghcr.io/josephrandall577/tavilyproxymanager:latest
    container_name: tavily-proxy
    ports:
      - "8080:8080"
    environment:
      - LISTEN_ADDR=:8080
      - DATABASE_PATH=/app/data/proxy.db
      - TAVILY_BASE_URL=https://api.tavily.com
      - UPSTREAM_TIMEOUT=30s
    volumes:
      - ./data:/app/data
      - /etc/localtime:/etc/localtime:ro
    restart: unless-stopped
```

执行启动：

```bash
docker-compose up -d
```

### 2. 使用 Docker 原生命令

```bash
docker run -d \
  --name tavily-proxy \
  -p 8080:8080 \
  -v $(pwd)/data:/app/data \
  -e DATABASE_PATH=/app/data/proxy.db \
  ghcr.io/josephrandall577/tavilyproxymanager:latest
```

---

## 🔑 首次运行：获取 Master Key

服务在**首次启动**时会自动生成一个随机的 **Master Key**，用于后续登录管理面板和调用 API。

您可以通过以下命令查看控制台日志来获取它：

```bash
docker logs tavily-proxy 2>&1 | grep "master key"
```

**日志示例：**
`level=INFO msg="no master key found, generated a new one" key=your_generated_master_key_here`

> **提示**：建议首次登录后在管理面板或通过数据库备份妥善保存此 Key。

---

## 🛠️ 本地开发与手动编译

如果您需要修改源码并自行构建：

1.  **启动后端**:
    ```bash
    go run ./server
    ```
2.  **启动前端**:
    ```bash
    cd web && npm install && npm run dev
    ```

**手动编译二进制产物**:

- **Windows**: `.\scripts\build_all.ps1`
- **Linux/macOS**: `./scripts/build_all.sh`

**使用 Dockerfile 构建镜像（Buildx）**:

首次使用可先初始化 Buildx：

```bash
docker buildx create --use
```

本地构建（当前主机架构）：

```bash
docker buildx build --load -t my-tavily-proxy .
```

构建并推送 `linux/amd64` 镜像：

```bash
docker buildx build \
  --platform linux/amd64 \
  -t ghcr.io/josephrandall577/tavilyproxymanager:latest \
  --push .
```

---

## 📖 使用指南

### REST API 代理

客户端调用方式与 Tavily 官方 API 完全一致，只需将 API 地址替换为代理地址，并使用 **Master Key**：

```bash
curl -X POST "http://localhost:8080/search" \
  -H "Authorization: Bearer <MASTER_KEY>" \
  -H "Content-Type: application/json" \
  -d '{"query": "最新 AI 技术趋势", "search_depth": "basic"}'
```

**兼容性说明**:

- 支持 Tavily Search、Extract、Crawl、Map、Research 及 Research 状态查询；Research 的 SSE 响应会实时转发。
- 支持 `{"api_key": "<MASTER_KEY>"}` 或 `{"apiKey": "<MASTER_KEY>"}`。
- 支持 GET 参数 `?api_key=<MASTER_KEY>`。

### MCP (Model Context Protocol)

服务在 `http://localhost:8080/mcp` 提供 HTTP MCP 端点。
内置与 Tavily 官方一致的 `tavily_search`、`tavily_extract`、`tavily_crawl`、`tavily_map` 和 `tavily_research` 工具，以及本项目扩展的 `tavily_usage`。`tavily_research` 会在服务端轮询或消费 SSE，并在单次工具调用中返回完整报告。

默认启用无状态模式（`MCP_STATELESS=true`），可避免客户端出现 `session not found`。
如需有状态会话，请将 `MCP_STATELESS=false`，并确保上游反向代理正确透传 `Mcp-Session-Id` 且启用会话粘性（sticky）。

#### Codex 配置示例（原生 Streamable HTTP）

Codex 原生支持 Streamable HTTP MCP 和 Bearer Token 鉴权，无需安装 `mcp-remote`。参见 [Codex MCP 官方说明](https://learn.chatgpt.com/docs/extend/mcp)。准备以下信息：

- MCP URL：本地部署使用 `http://localhost:8080/mcp`，公网部署使用 `https://您的域名/mcp`。
- Master Key：本项目首次启动时生成的 Master Key，不是 Tavily API Key。

以下两种鉴权方式任选其一。

**方式一：通过环境变量保存 Master Key（推荐）**

添加 MCP：

```bash
codex mcp add tavily-proxy \
  --url https://您的域名/mcp \
  --bearer-token-env-var TAVILY_PROXY_MASTER_KEY
```

启动 Codex 前设置环境变量。运行后粘贴 Master Key 并回车，密钥不会出现在 shell history 中：

```bash
read -s TAVILY_PROXY_MASTER_KEY
export TAVILY_PROXY_MASTER_KEY
```

macOS 从桌面启动 Codex 时，可写入当前登录会话的环境：

```bash
read -s TAVILY_PROXY_MASTER_KEY
launchctl setenv TAVILY_PROXY_MASTER_KEY "$TAVILY_PROXY_MASTER_KEY"
unset TAVILY_PROXY_MASTER_KEY
```

> `launchctl setenv` 仅对当前 macOS 登录会话有效，注销或重启系统后需要重新设置。

**方式二：直接写入 `config.toml`**

编辑全局配置 `~/.codex/config.toml`，或可信项目中的 `.codex/config.toml`：

```toml
[mcp_servers.tavily-proxy]
url = "https://您的域名/mcp"
http_headers = { Authorization = "Bearer 您的_MASTER_KEY" }
```

使用静态 Header 时，删除同一服务下的 `bearer_token_env_var`，避免维护两套凭据。Master Key 会以明文保存，建议限制全局配置文件权限：

```bash
chmod 600 ~/.codex/config.toml
```

**确认配置**

```bash
codex mcp get tavily-proxy
codex mcp list
```

完全退出并重新打开 Codex，然后新建任务。在 Codex 中输入 `/mcp`，应能看到：

- `tavily_search`
- `tavily_extract`
- `tavily_crawl`
- `tavily_map`
- `tavily_research`
- `tavily_usage`

调用示例：

```text
使用 tavily-proxy 搜索 Tavily 最新 API 变化，整理结论并附来源链接。
```

**常见问题**

- **配置已启用，但当前任务没有工具**：MCP 工具在任务启动时加载。完全退出并重启 Codex，再新建任务；旧任务不会动态获得新工具。
- **初始化返回 `401`**：Master Key 错误、已重置，或误用了 Tavily API Key。重新获取当前 Master Key 并更新配置。
- **`codex mcp list` 显示 enabled，仍无法调用**：该命令只能确认配置已加载，不能代替远端鉴权与初始化检查。优先检查 Master Key、MCP URL 和反向代理配置。
- **公网部署连接失败**：确认 URL 以 `/mcp` 结尾，并确保反向代理允许 MCP 请求且透传 `Authorization` Header。

#### VS Code 配置示例 (配合 mcp-remote)

```json
{
  "servers": {
    "tavily-proxy": {
      "command": "npx",
      "args": [
        "-y",
        "mcp-remote",
        "http://localhost:8080/mcp",
        "--header",
        "Authorization: Bearer 您的_MASTER_KEY"
      ]
    }
  }
}
```

---

## ⚙️ 配置项 (环境变量)

| 变量名             | 说明                 | 默认值                   |
| :----------------- | :------------------- | :----------------------- |
| `LISTEN_ADDR`      | 服务监听地址         | `:8080`                  |
| `DATABASE_PATH`    | SQLite 数据库路径    | `/app/data/proxy.db`     |
| `TAVILY_BASE_URL`  | 上游 Tavily API 地址 | `https://api.tavily.com` |
| `UPSTREAM_TIMEOUT` | 上游请求超时时间     | `150s`                   |
| `MCP_STATELESS`    | MCP 是否无状态模式   | `true`                   |
| `MCP_SESSION_TTL`  | MCP 会话空闲超时     | `10m`                    |

---

## 📄 开源协议

本项目基于 MIT 协议开源。
