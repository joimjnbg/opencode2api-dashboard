# 目标记录 (Goals)

本文件记录 opencode2api 这份对话中确立的目标与改进项，含完成状态。来源见相关提交与 issue。

## 完成 (Done)

- **上游模型目录自动刷新**：`StartModelRefresh` 在 openai 模式下每个周期自动从上游 `/v1/models` 拉取最新列表更新路由目录；静态列表退化为兜底，上游不可达时保留上次目录。提交 `95266a1`。
- **跨层自动回退（`failover.cross_tier_fallback_model`）**：主上游（zen/Google）全部 key 配额耗尽/冷却/节流时，自动改写模型到第二上游（go/lfree）的 `big-pickle` 继续服务。提交 `11a3d01`。
- **消除 `upstream_error: Bad Request`**：`sanitizeOpenAIBody` 重命名/剔除 Gemini 拒绝的字段（`max_completion_tokens`→`max_tokens`、`top_k`/`stop_sequences`/`candidate_count`/`max_output_tokens` 等大小写变体、`function_call` 等），并折叠顶层 `system`。提交 `49532ed`、`7c19efa`。
- **回退使用独立上下文**：主层背压不再 `context canceled` 掉回退请求。提交 `7c19efa`。
- **直连代理不再因上游超时被判不可用（本轮根因）**：`direct` 出口不会被单个上游超时标记 `unhealthy` 而连带禁用回退层；修掉了 Google 慢/宕时 lfree 明明可达却持续 503 的问题。提交 `5364de3`。
- **回退层（go/lfree）单 key 存活**：封顶指数退避、不再因上游 429/401/403 触发 30 分钟额度停用 / 10 分钟账号拒绝——只做短冷却持续重试。提交 `5364de3`。
- **主层回退前等待上限**：`min(retry.timeout_seconds/2, 10s)`，避免客户端在回退前断开。提交 `5364de3`。
- **生产级可观测性**：Prometheus 延迟直方图 + 重试计数；管理 API（刷新目录 / 查看 key 状态 / 覆写冷却 / 探测上游）；分层并发限流。提交 `d47cfcb`、`ef8d465`、`6a58368`。
- **完整文件日志**：`logging.file` 追加写 + stdout；请求入口与回退失败日志便于自诊断。提交 `5364de3`。
- **改进记录 issue**：https://github.com/joimjnbg/opencode2api-dashboard/issues/3

## 待办 / 待跟进 (Pending)

- **lfree 后端 `hy3` 映射异常**：lfree `/v1/models` 列出 `hy3`，但实际调用返回 `"Model hy3-free is not supported"`——是 lfree 后端问题，网关无法本地修复；需 lfree 侧修正或确认正确模型名。
- **`hy3-free` 未收录**：lfree 并不提供 `hy3-free`（403）。除非确认为新模型别名，否则不必加入目录。
- **Google（zen）回归验证**：本机当前 Google 不可达，zen 主路径未能回归；恢复后需确认 zen 正常、仅在配额耗尽时回退。
- **回退层瞬态 502 吸收**：lfree 偶发 502 目前直接透传；可考虑在回退层内做一次短重试。

## 维护惯例 (Maintenance)

- **模型目录**：已实现自动刷新，无需手动同步 `models.static_go`。静态列表仅作兜底；如上游长期不可达，可手动更新静态列表后重启。
- **开发流程**：每个新功能必须 TDD（先写测试再实现），完成后 rebuild + restart `opencode2api.exe`。
- **skills 更新**：经 `npx skills@latest add mattpocock/skills` 安装（项目级 `.agents/skills`），用 `npx skills@latest update -p -y` 拉取最新。
