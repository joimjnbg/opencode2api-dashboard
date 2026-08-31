# 更新记录 (Changelog)

本仓库遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 规范，版本号采用语义化版本 [SemVer](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 新增

- **跨层自动回退（`failover.cross_tier_fallback_model`）**：主上游（zen 层，如只有每日限额的 Google AI Studio）全部 key 配额耗尽/冷却/节流时，自动把请求改写到第二上游（go 层）的指定模型继续服务——客户端无感、用量仍记在原模型名下；两层都不可用才返回 503。留空禁用。
- **Gemini 请求字段清洗（消除 `upstream_error: Bad Request`）**：`sanitizeOpenAIBody` 现在还会剔除/改写 Gemini OpenAI 兼容端点拒绝的字段——`max_completion_tokens`/`max_output_tokens`→`max_tokens`（重命名）、`function_call`/`functions`/`metadata`/`reasoning_effort`/`service_tier` 直接剔除、`temperature` 钳到 `[0,2]`、顶层 `system` 折叠进 messages 的 system 角色；并把 Gemini 原生 GenerateContent 参数也一并剔除（`top_k`、`stop_sequences`、`candidate_count`、`safety_settings`、`response_schema`、`generation_config` 等大小写变体）——这些正是 opencode 为 Gemini 模型发出的、却让 OpenAI 兼容端点返回 400 的元凶。

- **跨层回退使用独立上下文**：当主层（zen）全部 key 冷却/节流、请求回退到第二上游（go/lfree）时，改为用独立超时上下文发起回退请求，避免主层背压等待耗尽了请求预算导致回退请求被 `context canceled` 而失败（此前表现为 `all upstream keys are temporarily unavailable` 503）。

- **瞬时上游故障整池重试**：一轮 key 池全部失败且属瞬态错误（上游 502/503/504 或传输错误）时，自动等待 key 冷却到期后重试整池（受 `retry.max_attempts` 与请求超时双重约束），4xx 请求形状错误不重试。对单 key 层（无下一个 key 可切换）尤为关键——上游偶发超时不再直接透传给客户端。
- **双上游池（openai 模式）**：新增 `models.static_go` 静态目录，第二上游（`upstream.go` + `go_keys`）与主上游（`upstream.zen` + `zen_keys` + `models.static`）同时服务：按模型名路由到所属层，两层各自维护独立的 key 池、静默 failover、账号级熔断、配额停用与 Prometheus 指标（`tier="zen"|"go"`）。一个上游限流时另一个继续承担流量；两层模型名不应重复，`prefer` 仅在同名模型时生效。
- **OpenAI 兼容上游（`upstream_mode: "openai"`）**：支持接入任意 OpenAI 兼容上游（如 Google AI Studio / Gemini，基址 `https://generativelanguage.googleapis.com/v1beta/openai`）。该模式下：不再发送 opencode 客户端头（`x-opencode-*`、`x-machine-id` 等）；放行 `gemini-*` 模型；模型目录改用 `models.static` 静态列表（避免依赖非 OpenAI 形态的 `/models` 端点）；429 仅按单 key 节流而非额度停用；配额识别改用 OpenAI 兼容规则（`rate_limit_exceeded`、配额短语，不含裸 `429`）。原 opencode.ai 行为在 `upstream_mode: "opencode"`（默认）下完全不变。
- **多账号池（`failover.multi_account`）**：适用于一个池内配置了多个不同 opencode 账号 key 的场景。每个 key 视为独立账号——429 只冷却/限流该账号本身，不再误判整个池为共享账号限流；请求按轮转公平地分摊到各账号（不再把整段对话钉在单一账号上，避免某账号额度先耗尽）。
- **额度耗尽停用（`failover.quota_park_minutes`）**：多账号模式下，账号确认触发免费额度上限后按窗口停用（默认 1440 分钟，即一天）直到额度窗口重置，而不是仅短暂冷却反复空转；窗口到期后自动重新参与轮转，恢复即回池。
- **新状态与指标**：`/healthz` 每个 key 的状态新增 `parked`（额度停用）与逐账号 `throttled`；新增 Prometheus 指标 `opencode2api_keys_throttled_total`、`opencode2api_keys_parked_total`。

### 变更

- `isQuotaError`、`supportedModel`、`protocolPath` 改为按 `upstream_mode` 区分行为；删除未使用的死代码 `errorBodySanitized`。

### 修复

- **回退上下文保留客户端取消**：跨层回退的独立超时现在从原始请求上下文派生（而非 `context.Background()`），客户端断开时回退请求也会及时终止，不再浪费资源。
- **`Retry-After` 不再能绕过冷却上限**：`MarkFailure` 中 `maxCooldown` 上限现在在 `Retry-After` 之后应用，确保上游返回过大 `Retry-After`（或无限重试）时单 key 仍会在基准窗口内恢复——回退层尤为关键。
- **`PruneOlderThan` 已接入定时器**：`usageStats` 现在每小时自动清理 48 小时前的小时级统计，防止长期运行的网关内存无限增长。
- **删除死代码 `sanitizeRequestModel`**：该函数未被调用，模型清洗已由 `routeWithSanitize`/`sanitizeModel` 处理。
- **直连代理不再因上游超时而被判不可用**：`direct`（无代理直出）出口本身没有可“宕机”的中间节点，`isProxyFailure` 原把上游超时/连接拒绝也视作代理故障，导致任一上游（如 Google）慢或宕机时把共享的 `direct` 代理整体标记 `unhealthy`，连带禁用回退层（go）所有 key——表现为即便 lfree 可达也持续 `all upstream keys are temporarily unavailable` 503。现在 `direct` 代理永远不会因上游/探活失败被判不可用，上游故障改由 key 冷却处理。
- **回退层（go）key 不会被长冷却拖垮**：回退层是唯一兜底上游，其单 key 不再累加指数退避（封顶为基准冷却），也不再因上游 429/401/403 触发 30 分钟额度停用或 10 分钟账号拒绝——只做短冷却并持续重试，确保兜底始终在线；主层（zen）行为不变。
- **回退主层等待上限**：主层（zen）在回退前最多等待 `min(retry.timeout_seconds/2, 10s)`，避免把整个请求超时耗在已宕主层上、客户端在回退前就断开；回退请求仍用独立完整超时上下文。
- **完整文件日志**：`logging.file` 可配置日志文件路径（追加写入），与 stdout 同时输出（`io.MultiWriter`）；`handleInference` 在请求入口记录 `model/tier/protocol/external/stream`，跨层回退失败记录 `fallback_error` 与 `primary_error`，便于自诊断而不依赖使用者提供信息。

## [v3.1.0] - 2026-08-15

### 新增

- **账号级熔断（共享 429 检测）**：实测确认 OpenCode 的限流按账号/工作区共享（一个 key 打爆 429 后同账号其他 key 立即 429，且无 `Retry-After`/剩余额度响应头）。网关现在跟踪每个 key 最近一次 429，短窗口内不同 key 的 429 数达到 `failover.throttle.shared_429_threshold`（默认 2）即判定账号级共享限流，整个 tier 的 key 池进入节流窗口。
- **背压等待（TCP 拥塞退避）**：节流窗口 60 秒起、每次复发翻倍、上限 600 秒（`initial_seconds`/`max_seconds`）；窗口内请求**等待**（`max_wait_seconds`，默认 60 秒）而不是反复打击 key，到期自动半开探测，成功即解除并清零窗口；等待超时返回 503 + `Retry-After`，客户端可按头重试。
- **新指标** `opencode2api_account_throttled{tier="zen"|"go"}`：账号级节流是否激活（1/0）。
- **healthz key 状态**：新增 `zen_status`/`go_status`（working/reject/throttled/cooling）、`throttled`、`throttle_in_seconds` 字段。

### 变更

- `doUpstream` 重构为带背压的请求循环（`requestLoop`）：节流等待 → 候选 key 排序 → 全冷却等待 → 有界 deadline。
- 新配置段 `failover.throttle`（四个字段，均有合理默认值，可不配）。

### 修复

- **节流窗口过期后死循环**：`ThrottleDeadline` 在窗口已过期时返回零值（视为未节流），避免 `sleepCtx` 负时长立即返回导致的忙循环。

### 新增测试

- `TestShared429BackpressureThrottlesThenRecovers`：共享 429 → 背压等待 → 半开探测成功 → 节流解除（完整链路）。
- `TestShared429BeyondWaitReturnsThrottleError`：节流窗口超出等待预算 → `throttleError`（503 + Retry-After）。
- `TestRecord429Threshold`：共享限流检测器阈值逻辑（单元级）。
- `TestMarkAccountThrottledExponential`：节流窗口指数翻倍与成功解除（单元级）。

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