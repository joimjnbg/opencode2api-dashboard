// opencode2api usage plugin
//
// 提供两个能力：
//  1. 自定义工具 query_usage —— AI 在会话中可主动查询网关用量（token/cost/请求数）
//  2. session.idle 钩子 —— 每次会话空闲时在日志输出用量摘要
//
// 环境变量（也可在项目/全局 .env 中配置）：
//  OPENCODE2API_STATS      用量端点，默认 http://127.0.0.1:8080/v1/stats
//  OPENCODE2API_LOCAL_KEY  本地 API key（必填，用于鉴权 /v1/stats）
//
// 安装：把本文件复制到 ~/.config/opencode/plugins/ 即可自动加载。

const DEFAULT_STATS_URL = "http://127.0.0.1:8080/v1/stats";

function fmtCost(n) { return n ? `$${Number(n).toFixed(4)}` : "$0.0000"; }
function fmtTokens(n) {
  n = Number(n) || 0;
  if (n >= 1e9) return (n / 1e9).toFixed(2) + "B";
  if (n >= 1e6) return (n / 1e6).toFixed(2) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(1) + "K";
  return String(n);
}

async function fetchUsage(key) {
  const url = process.env.OPENCODE2API_STATS || DEFAULT_STATS_URL;
  const headers = {};
  if (key) headers["Authorization"] = `Bearer ${key}`;
  const r = await fetch(url, { headers });
  if (!r.ok) throw new Error(`usage endpoint HTTP ${r.status}`);
  return r.json();
}

function renderSummary(data) {
  const t = data.total || {};
  const okRate = t.requests ? Math.round((t.success / t.requests) * 100) : 0;
  const top = (data.models || [])
    .slice()
    .sort((a, b) => (b.stats.input_tokens + b.stats.output_tokens) - (a.stats.input_tokens + a.stats.output_tokens))
    .slice(0, 5)
    .map((m) => `  - ${m.model}: ${fmtTokens(m.stats.input_tokens + m.stats.output_tokens)} tokens, ${m.stats.requests} req, ${fmtCost(m.stats.cost)}`)
    .join("\n");
  return [
    `请求 ${t.requests} (成功 ${t.success} / 失败 ${t.failed} / ${okRate}%)`,
    `Input ${fmtTokens(t.input_tokens)} (cached ${fmtTokens(t.cached_tokens)}) | Output ${fmtTokens(t.output_tokens)} (reasoning ${fmtTokens(t.reasoning_tokens)})`,
    `成本 ${fmtCost(t.cost)}`,
    top ? `Top 模型:\n${top}` : "",
  ].filter(Boolean).join("\n");
}

export const Opencode2apiUsagePlugin = async ({ client, project }) => {
  const key = process.env.OPENCODE2API_LOCAL_KEY || "";
  if (!key) {
    await client.app.log({
      body: { service: "opencode2api-usage", level: "warn", message: "OPENCODE2API_LOCAL_KEY not set; query_usage tool will fail until configured" },
    });
  }

  return {
    tool: {
      query_usage: {
        description:
          "Query the local opencode2api gateway usage statistics: total requests, success/failure counts, input/output/cached/reasoning tokens, monetary cost, and the top models by token usage.",
        args: {},
        async execute() {
          try {
            const data = await fetchUsage(key);
            return renderSummary(data);
          } catch (e) {
            return `Failed to fetch usage: ${e.message}`;
          }
        },
      },
    },
    event: async ({ event, client: evClient }) => {
      if (event.type === "session.idle") {
        try {
          const data = await fetchUsage(key);
          await evClient.app.log({
            body: {
              service: "opencode2api-usage",
              level: "info",
              message: "session usage summary",
              extra: { project: project?.id, summary: renderSummary(data) },
            },
          });
        } catch { /* silent: gateway may be down */ }
      }
    },
  };
};
