# opencode2api-dashboard

**opencode2api-dashboard** 是 [opencode2api](https://github.com/jasonxu114514/opencode2api) 的增强分支：在保留原版全部能力（OpenCode Zen / Zen Go 协议代理、多 key 池、多代理调度、协议转换）的基础上，新增 **token/cost 用量统计**与**图形化实时监控仪表盘**。

`opencode2api` 使用 Go 编写，对外提供标准 OpenAI 与 Anthropic API，并自动添加 OpenCode 客户端请求头，让任何 OpenAI/Anthropic 兼容客户端都能使用 OpenCode Zen 的模型。

## 本分支新增功能

### 1. 内置用量统计 `/v1/stats`（带鉴权）

网关在内存中按**模型**和**小时**聚合真实用量数据（来自上游响应的 `usage` 与 `cost` 字段）：

```json
GET /v1/stats
Authorization: Bearer <你的本地 server key>
```

```json
{
  "uptime_seconds": 3600,
  "total": { "requests": 20, "success": 20, "failed": 0,
             "input_tokens": 3022430, "output_tokens": 8639,
             "cached_tokens": 3009024, "reasoning_tokens": 275,
             "cost": 0 },
  "models": [ { "model": "deepseek-v4-flash-free", "stats": { ... } } ],
  "hours":  [ { "hour": "2026-08-15T03:00:00Z", "stats": { ... } } ]
}
```

- 非流式响应与 SSE 流式响应都会记录（流式在结束 chunk 解析 usage）。
- 失败请求也会计数（`failed`），便于观察 key/代理故障。
- 统计仅存内存，进程重启后归零，适合网关长期运行的场景。

### 2. 图形化仪表盘（Node，零依赖）

`dashboard.mjs` 启动一个本地 HTTP 服务，浏览器打开即可查看：

| 面板 | 内容 |
| --- | --- |
| 状态卡片 | 网关状态、模型数、Key 池、代理健康、版本 |
| 用量卡片 | 总请求/成功率、总成本、Input/Output/Reasoning tokens、运行时长 |
| 请求趋势 | 最近 30 分钟请求量柱状图 |
| Token 趋势 | 最近 24 小时小时级 token 用量 |
| 模型排行 | 按 token 用量排序的模型 TOP 榜 |
| 失败分布 | HTTP 4xx/5xx 状态统计 |
| 实时日志 | 网关 debug 日志滚动展示（事件、tier、代理、状态） |

```bash
export OPENCODE2API_LOCAL_KEY=sk-your-local-key   # 读取 /v1/stats 的本地密钥
node dashboard.mjs                                 # 打开 http://127.0.0.1:9090
```

也支持 `.env` 文件配置：

```
OPENCODE2API_LOCAL_KEY=sk-your-local-key
OPENCODE2API_DASHBOARD_PORT=9090
```

## 原版功能

- 支持 OpenAI Chat Completions、Responses 和 Models API
- 支持 Anthropic Messages API
- 支持普通响应和 SSE 流式响应
- 支持文本、图片、thinking/reasoning、工具定义、工具调用和工具结果转换
- 分离配置 Zen key 池与 Zen Go key 池
- 模型同时存在于两个上游时按 `prefer` 配置优先使用 Go 或 Zen（默认 Go）
- 支持直连、HTTP、HTTPS、SOCKS5 和 SOCKS5H 代理
- 将 key 自动均衡绑定到代理，保持连接亲和性
- 使用稳定会话哈希保持同一会话的 key/proxy 亲和性，并在节点故障时自动回退
- 代理失败后自动迁移绑定，key 失败后进行短时冷却
- 根据真实上游流量识别代理故障，并每 15 分钟通过 Cloudflare trace 并行复查异常代理
- 为不同会话生成不同的 OpenCode 会话 ID，并支持 `x-opencode-session`、`x-session-id` 和 `conversation-id` 显式指定会话

## API 路径

| 方法 | 路径 | 协议 |
| --- | --- | --- |
| `GET` | `/v1/models` | OpenAI 模型列表 |
| `POST` | `/v1/chat/completions` | OpenAI Chat Completions |
| `POST` | `/v1/responses` | OpenAI Responses |
| `POST` | `/v1/messages` | Anthropic Messages |
| `GET` | `/v1/stats` | 用量统计（需鉴权） |
| `GET` | `/healthz` | 健康检查 |

## 编译

需要 Go 1.24 或更高版本。

```bash
go build -o opencode2api ./
```

## Docker Compose 部署

```bash
git clone <本仓库>
cd opencode2api-dashboard
docker compose up -d

cp config.example.json config.json
# 编辑 config.json 中的 server_keys、zen_keys 或 go_keys
docker compose restart
```

## 配置

复制示例配置并编辑 `config.json`：

```json
{
  "listen": "127.0.0.1:8080",
  "server_keys": ["change-this-local-key"],
  "zen_keys": ["sk-your-zen-key"],
  "go_keys": [],
  "prefer": "go",
  "proxies": ["direct"],
  "upstream": {
    "zen": "https://opencode.ai/zen",
    "go": "https://opencode.ai/zen/go"
  },
  "retry": { "max_attempts": 3, "timeout_seconds": 300 },
  "models": { "refresh_seconds": 300, "protocols": {} },
  "performance": {
    "max_idle_conns": 2048, "max_idle_conns_per_host": 256,
    "max_conns_per_host": 0, "idle_conn_timeout_seconds": 120,
    "connect_timeout_seconds": 5, "failure_cooldown_seconds": 15
  },
  "logging": { "level": "info" }
}
```

字段含义与原版一致：

| 字段 | 含义 |
| --- | --- |
| `listen` | 本地监听地址，建议 `127.0.0.1:8080` 避免暴露公网 |
| `server_keys` | 调用本代理的本地 API key（只用于本地鉴权） |
| `zen_keys` | OpenCode Zen API key 池，可多个 |
| `go_keys` | OpenCode Zen Go API key 池 |
| `prefer` | 模型同时存在于 Zen 与 Go 时的优先上游（`go`/`zen`） |
| `proxies` | 上游代理：`direct`、`http://`、`https://`、`socks5://`、`socks5h://` |

## 会话 ID

代理会为上游添加 OpenCode 使用的 `User-Agent`、`x-opencode-client`、`x-opencode-session`、`x-opencode-request` 和 `x-opencode-project` 请求头。

- 每个请求使用不同的 `x-opencode-request`，同一次请求的重试保持不变。
- 优先使用客户端提供的 `x-opencode-session`、`x-session-id`、`conversation-id`、`conversation_id` 或 `metadata.session_id` 生成会话 ID。
- 没有显式会话标识时，使用第一条用户消息生成稳定会话 ID。

## 致谢

感谢 [LINUX DO](https://linux.do) 社区一直以来的支持。

上游项目：[jasonxu114514/opencode2api](https://github.com/jasonxu114514/opencode2api)。原仓库未附带 LICENSE，本分支按 MIT 协议发布新增代码；使用原版代码时请自行确认合规性。
