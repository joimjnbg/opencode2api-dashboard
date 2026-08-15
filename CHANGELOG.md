# 更新记录 (Changelog)

本仓库遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 规范，版本号采用语义化版本 [SemVer](https://semver.org/lang/zh-CN/)。

## [Unreleased]

- 暂无

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