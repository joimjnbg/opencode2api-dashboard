# opencode2api-dashboard

[![CI](https://github.com/joimjnbg/opencode2api-dashboard/actions/workflows/ci.yml/badge.svg)](https://github.com/joimjnbg/opencode2api-dashboard/actions/workflows/ci.yml)
![版本](https://img.shields.io/badge/version-v3.0.0-blue)
![License](https://img.shields.io/badge/license-MIT-green)

**opencode2api-dashboard** 是 [opencode2api](https://github.com/jasonxu114514/opencode2api) 的增强分支：在保留原版全部能力（OpenCode Zen / Zen Go 协议代理、多 key 池、多代理调度、协议转换）的基础上，新增 **token/cost 用量统计**、**JSONL 审计持久化**、**Prometheus 指标**与**图形化实时监控仪表盘**。

> 更新记录见 [CHANGELOG.md](CHANGELOG.md)。

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
- 统计仅存内存，进程重启后归零；如需持久化请开启下面的审计日志。

### 2. JSONL 审计日志（持久化）

在 `config.json` 中启用后，每条完成的请求会追加一行 JSON 到文件（默认 `opencode2api.audit.jsonl`），重启不丢失，可被仪表盘/脚本按天聚合：

```json
{ "cost": 0, "model": "deepseek-v4-flash-free", "ok": true, "ts": "2026-08-15T03:59:02Z", "usage": { "Input": 84, "Output": 8, "Total": 92, "Cached": 0, "Reasoning": 0 } }
```

```json
"stats": { "audit_file": "opencode2api.audit.jsonl" }
```

### 3. Prometheus 指标 `/metrics`

零依赖实现 Prometheus 文本格式，可直接接入 Grafana / VictoriaMetrics / Prometheus：

```
GET /metrics
opencode2api_requests_total / requests_success_total / requests_failed_total
opencode2api_tokens_input_total / output_total / cached_total / reasoning_total
opencode2api_cost_total        # 均带 model 标签
opencode2api_proxies_healthy / proxies_total
opencode2api_keys_zen / keys_go
opencode2api_keys_cooling_total # 当前冷却中的 key 数（tier 标签）
opencode2api_up / opencode2api_uptime_seconds
```

### 4. 图形化仪表盘（Node，零依赖）

`dashboard.mjs` 启动一个本地 HTTP 服务，浏览器打开即可查看：

| 面板 | 内容 |
| --- | --- |
| 状态卡片 | 网关状态、模型数、Key 池、代理健康、版本 |
| 用量卡片 | 总请求/成功率、总成本、Input/Output/Reasoning tokens、成本阈值告警状态 |
| 请求趋势 | 最近 30 分钟请求量柱状图 |
| Token 趋势 | 最近 24 小时小时级 token 用量 |
| 历史趋势 | 审计日志按天聚合的历史用量（重启不丢） |
| 模型排行 | 按 token 用量排序的模型 TOP 榜 |
| 失败分布 | HTTP 4xx/5xx 状态统计 |
| 实时日志 | 网关 debug 日志滚动展示（事件、tier、代理、状态） |
| 多实例聚合 | 通过 `OPENCODE2API_INSTANCES` 聚合多台网关的用量与健康 |
| 告警 | 成本阈值 / 失败率阈值 / 实例不可达，触发 webhook（Telegram 等） |
| 模型选择 | 实时拉取网关模型列表，勾选启用 + 设置默认模型，保存写入 opencode 配置 |
| 配置管理 | 可视化编辑 API Key（掩码显示、自动还原），目标可选网关/全局/用户配置，保存自动备份并重启 |
| 一键重启 | 设置页重启网关按钮（带确认） |
| Key 测试 | 粘贴本地 key 验证有效性 |
| 请求明细 | 最近请求审计明细（时间/模型/结果/token/成本） |

```bash
export OPENCODE2API_LOCAL_KEY=sk-your-local-key   # 读取 /v1/stats 的本地密钥
node dashboard.mjs                                 # 打开 http://127.0.0.1:9090
```

也支持 `.env` 文件配置：

```
OPENCODE2API_LOCAL_KEY=sk-your-local-key
OPENCODE2API_DASHBOARD_PORT=9090
```

### 4.1 多实例聚合

多台网关（不同机器/端口）时，聚合所有实例的统计与健康：

```
OPENCODE2API_INSTANCES='[{"name":"gw1","stats":"http://host1:8080/v1/stats","key":"sk-...","health":"http://host1:8080/healthz"},{"name":"gw2","stats":"http://host2:8080/v1/stats","key":"sk-..."}]'
```

### 4.2 告警（webhook）

在 `.env` 配置，超过阈值触发 webhook（默认 60 秒内去重）：

```
OPENCODE2API_ALERT_WEBHOOK=https://api.telegram.org/bot<TOKEN>/sendMessage   # 或任意 webhook
OPENCODE2API_ALERT_COST_LIMIT=1.5            # 累计成本超 $1.5 告警
OPENCODE2API_ALERT_FAILURE_RATE=0.3          # 失败率超 30% 告警
OPENCODE2API_ALERT_INTERVAL=60               # 去重间隔秒
```

Telegram 用法：URL 追加 `?chat_id=xxx&text={message}`（`{message}` 会被替换）；其他 webhook 直接 POST JSON。

### 4.3 模型选择页

仪表盘“模型”标签页：点击“刷新列表”**每次实时**从网关 `/v1/models` 获取模型列表（不缓存），可搜索过滤；勾选要启用的模型、设置默认模型，点“保存模型选择”写入 **opencode 全局配置或用户配置**（下拉选择目标），自动更新 `provider.zen2api.models` 与 `model` 字段，opencode 客户端重启后即可自由选择新模型。

### 4.4 设置页（配置管理 / Key 可视化）

仪表盘“设置”标签页：

- **配置目标**：网关 `config.json` / opencode 全局（`~/.config/opencode/opencode.json`）/ opencode 用户（项目 `opencode.json`），自由切换。
- **API Key 编辑**：JSON 编辑器，key 以掩码显示（`sk-G****HHR7`），保存时自动还原真实值，浏览器不暴露明文；opencode 用户配置不存在时自动创建。
- **保存**：写盘前自动备份 `config.json.bak`；保存网关配置后**自动重启网关**生效。
- **重启网关**：一键重启（带确认，重启期间请求短暂中断）。
- **Key 测试**：粘贴本地 server key 验证有效性（HTTP 200/401）。
- **最近请求明细**：审计日志最近 20/50/100 条（时间/模型/结果/token/成本）。

### 4.5 智能代理：静默重试 / 指纹伪装 / 模型清洗（v3）

网关作为“智能代理网关”运行，客户端对多账号限流与切换完全无感：

- **静默 Failover（错误捕获与无感重试）**：上游返回配额错误（`Free usage exceeded`、`rate_limit_exceeded`、402、429 等）时，自动将该 key 冷却（默认 30 分钟，`failover.quota_cooldown_minutes`）并**静默切换下一个健康 key 重试**，客户端不收到任何错误数据；所有 key 均冷却时才返回 503（`"all upstream keys are cooling down..."`），重试请求时节点自动重新参与。
- **账号级拒绝（401/403）长冷却**：无效 key、未绑定支付方式等稳定拒绝会把该 key 冷却 10 分钟（`accountRejectCooldown`），避免每 15 秒反复打击同一 key；其余 key 继续承担流量，客户端拿到真实错误仅在所有 key 都被拒绝时。
- **指纹伪装**：`fingerprint.enabled` 开启后，为每个 key 生成**独立且持久**（按 key 哈希存于 `fingerprints.json`，重启不变）的 `x-machine-id` / `vscode-machine-id` 设备指纹，上游按设备维度限流时互不影响。
- **模型清洗**：`sanitize.model_aliases` 支持模型重映射（如免费→付费模型路由）。⚠️ `strip_free_suffix` 默认 **false**：在 opencode.ai 上 `-free` 后缀就是免费模型的身份标识，剥离后请求会打到付费模型，无支付方式的账号将收到 401 `No payment method`。
- **主动限速轮换**：`rate_limit.proactive` 开启时，上游 `x-ratelimit-remaining` 低于 `rotate_at_remaining` 会提前把 key 短冷却，优先使用其他 key。
- **启动自愈**：端口被占（重启竞态）时不再退出，自动退避重试绑定，服务自愈恢复。

新增配置段（`config.json`）：

```json
"sanitize":    { "enabled": true, "strip_free_suffix": false, "model_aliases": {} },
"failover":    { "enabled": true, "quota_cooldown_minutes": 30, "treat_generic_429_as_quota": false },
"fingerprint": { "enabled": true, "persist_file": "fingerprints.json" },
"rate_limit":  { "enabled": true, "proactive": true, "rotate_at_remaining": 2 }
```

### 5. 终端 CLI `oc-stats.mjs`（零依赖）

不开浏览器也能看用量：

```bash
export OPENCODE2API_LOCAL_KEY=sk-your-local-key
node oc-stats.mjs              # 表格
node oc-stats.mjs --watch      # 每 5 秒刷新
node oc-stats.mjs --json       # 原始 JSON（供脚本/告警）
node oc-stats.mjs --top 5      # 只看 token 前 5 的模型
```

### 6. opencode 客户端插件（可选）

复制 `opencode2api-usage-plugin.mjs` 到 `~/.config/opencode/plugins/`（Windows: `%USERPROFILE%\.config\opencode\plugins\`）。插件为 AI 提供 `query_usage` 工具（会话中可查询网关用量），并在每次会话结束时输出用量摘要。需要环境变量 `OPENCODE2API_LOCAL_KEY`。

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
| `GET` | `/metrics` | Prometheus 指标 |
| `GET` | `/healthz` | 健康检查 |

## 编译

需要 Go 1.24 或更高版本。

```bash
go build -o opencode2api ./
# 带版本号
go build -ldflags "-X main.version=v1.2.3" -o opencode2api ./
```

## 一键启动（Windows）

```powershell
powershell -ExecutionPolicy Bypass -File start.ps1          # 启动网关 + 仪表盘
powershell -ExecutionPolicy Bypass -File start.ps1 -Stop    # 停止
powershell -ExecutionPolicy Bypass -File start.ps1 -Restart # 重启
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
