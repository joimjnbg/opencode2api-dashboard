# 更新记录 (Changelog)

本仓库遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 规范，版本号采用语义化版本 [SemVer](https://semver.org/lang/zh-CN/)。

## [Unreleased]

- 暂无

## [v3.0.0] - 2026-08-15

### 新增

- **静默 Failover（错误捕获与无感重试）**：网关升级为智能代理——上游返回配额错误（`Free usage exceeded` / `rate_limit_exceeded` / 402 / 429 等）时自动将该 key 冷却（`failover.quota_cooldown_minutes`，默认 30 分钟）并**静默切换下一个健康 key 重试**，客户端全程无感；全部 key 冷却时返回 503 中性提示。
- **账号级拒绝长冷却**：401/403（无效 key、无支付方式）将 key 冷却 10 分钟，避免反复打击失效 key，其余 key 继续承担流量。
- **指纹伪装**：`fingerprint.enabled` 为每个 key 生成独立、持久（按 key 哈希存 `fingerprints.json`，重启不变）的 `x-machine-id` / `vscode-machine-id`，规避上游设备维度限流。
- **模型清洗与重映射**：`sanitize.model_aliases` 支持模型重映射；`sanitize.strip_free_suffix` 默认 **false**（剥离 `-free` 后缀会把免费模型变成付费模型，无支付方式账号将 401）。
- **主动限速轮换**：`rate_limit.proactive` 按上游 `x-ratelimit-remaining` 提前短冷却 key 轮换。
- **启动自愈**：绑定端口失败（重启竞态）时自动退避重试，不再进程退出。
- **新指标** `opencode2api_keys_cooling_total{tier=...}`：当前冷却中的 key 数。

### 变更

- `config.example.json` 与默认配置新增 `sanitize` / `failover` / `fingerprint` / `rate_limit` 四段配置。
- `healthz` 与 `/metrics` 反映 key 冷却状态。
- 会话亲和性改为基于 FNV 哈希的稳定轮转（`ActiveOrder`），冷却 key 自动剔除。

### 修复

- **剥离 `-free` 后缀导致上游 401 "No payment method"**（`strip_free_suffix` 误开启）：默认关闭并修正示例配置。
- **网关进程反复自动终止**（bind 失败即退出）：改为退避重试绑定，重启竞态不再产生空窗期（ECONNREFUSED）。
- **失效 key 每 15 秒被反复重试**：401/403 改为 10 分钟长冷却。

## [v2.1.0] - 2026-08-15

### 新增

- **模型选择页**：仪表盘新增“模型”标签页，每次点击“刷新列表”实时从网关 `/v1/models` 获取模型列表（不缓存），支持搜索过滤、勾选启用模型、设置默认模型，一键保存写入 opencode 全局或用户配置（自动更新 `provider.zen2api.models` 与 `model` 字段）。
- **配置管理页（可视化设置 API Key）**：仪表盘新增“设置”标签页，配置目标自由选择——网关 `config.json` / opencode 全局配置（`~/.config/opencode/opencode.json`）/ opencode 用户配置（项目 `opencode.json`）。
  - API key 以掩码显示（如 `sk-G****HHR7`），保存时自动还原真实值，浏览器不暴露明文。
  - 保存前自动备份 `config.json.bak`，防止误配损坏配置。
  - 保存网关配置后自动重启网关使其生效；opencode 配置目标不存在时自动创建。
- **重启按钮**：设置页一键重启网关进程（带确认，重启期间请求短暂中断）。
- **本地 Key 测试**：粘贴本地 server key 即可验证有效性（调用 `/v1/models` 检查 HTTP 200/401）。
- **最近请求明细**：审计日志单条查询 `/api/audit-recent`，设置页展示最近 20/50/100 条请求（时间/模型/结果/token/成本）。

### 新增 API（仪表盘）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/models` | 实时获取网关模型列表（不缓存） |
| `GET` | `/api/config?target=gateway\|opencode-global\|opencode-user` | 读取配置（key 脱敏） |
| `POST` | `/api/config?target=...` | 保存配置（自动备份；网关目标自动重启） |
| `POST` | `/api/restart` | 重启网关进程 |
| `POST` | `/api/test-key` | 测试本地 key 有效性 |
| `GET` | `/api/audit-recent?limit=N` | 审计明细（默认 50，最多 500） |

### 变更

- 仪表盘改为三页布局：概览 / 模型 / 设置。

## [v2.0.0] - 2026-08-15

### 新增

- **Prometheus 指标** `GET /metrics`：零依赖实现 Prometheus 文本格式，输出请求数、成功率、token（input/output/cached/reasoning）、成本（均带 model 标签）、代理健康数、key 池数量、up/uptime，可直接接入 Grafana / Prometheus / VictoriaMetrics。
- **JSONL 审计日志**：`config.json` 的 `stats.audit_file` 配置后，每条完成的请求追加一行 JSON（cost/model/ok/ts/usage）到文件，重启不丢失，可按天聚合历史用量。
- **仪表盘多实例聚合**：通过 `OPENCODE2API_INSTANCES` 环境变量聚合多台网关的健康与用量统计。
- **仪表盘告警**：`OPENCODE2API_ALERT_WEBHOOK` / `OPENCODE2API_ALERT_COST_LIMIT` / `OPENCODE2API_ALERT_FAILURE_RATE` / `OPENCODE2API_ALERT_INTERVAL` 配置成本阈值、失败率阈值、实例不可达告警，支持 Telegram 与通用 webhook，默认 60 秒去重。
- **仪表盘审计历史视图**：`/api/audit` 接口 + 前端按天历史用量柱状图。
- **终端 CLI `oc-stats.mjs`**：零依赖，支持表格 / `--watch` 每 5 秒刷新 / `--json` 脚本输出 / `--top N`，支持多实例聚合。
- **opencode 客户端插件 `opencode2api-usage-plugin.mjs`**：提供 `query_usage` 自定义工具，AI 会话中可查询网关用量；`session.idle` 时输出会话用量摘要。复制到 `~/.config/opencode/plugins/` 即生效。
- **一键启动脚本 `start.ps1`**：Windows 下启动/停止/重启网关与仪表盘，自动跳过已在运行的服务。
- **GitHub Actions CI**（`.github/workflows/ci.yml`）：gofmt 检查、go vet、go build、Node 语法检查、配置示例校验，push/PR 自动执行。
- **仪表盘成本阈值状态展示**：前端显示告警阈值配置与启用状态。

### 变更

- `config.example.json` 新增 `stats.audit_file` 配置示例（默认 `opencode2api.audit.jsonl`）。
- `dashboard.mjs` 完全重写：`.env` 加载、多实例聚合、告警模块、审计历史接口；`/api/health` 返回结构改为 `instances` 数组。
- `dashboard.html` 重写：适配新版 API（实例卡片、审计历史图、监控端点说明）。
- 构建支持 `-ldflags "-X main.version=<tag>"` 注入版本号。

### 修复

- 无（v2.0.0 基于 v1 功能扩展）。

## [v1.0.0] - 2026-08-14

### 新增

- **内置用量统计** `GET /v1/stats`（带鉴权）：内存中按模型与小时聚合真实用量（requests/success/failed/input/output/cached/reasoning tokens/cost）。
- **统计采集**：非流式响应与 SSE 流式响应均解析 `usage` 与 `cost`，失败请求计数。
- **图形化仪表盘 v1**（`dashboard.mjs` + `dashboard.html`）：状态卡片、用量卡片、请求趋势、Token 趋势、模型排行、失败分布、实时日志滚动。
- 项目开源化：README 重写、MIT LICENSE、.gitignore 清理。

### 变更

- 基于上游 [jasonxu114514/opencode2api](https://github.com/jasonxu114514/opencode2api) 分叉，保留原版全部代理能力。

---

## 版本历史 (v1 之前，继承上游)

- `e68a0b9` fix: normalize tool reasoning history across protocols
- `1c149e3` fix: normalize Anthropic tool thinking history
- `52f19b7` fix: preserve reasoning and add session affinity
- 其余为上游原始提交，见 git log。