#!/usr/bin/env node
// oc-stats —— 终端查看 opencode2api 用量统计（含多实例聚合与 Kibana/Grafana 之外的轻量视图）
//
// 用法：
//   node oc-stats.mjs                    # 单实例（读 .env 或环境变量）
//   node oc-stats.mjs --watch            # 每 5 秒刷新一次表格
//   node oc-stats.mjs --json             # 输出原始 JSON（供脚本/告警使用）
//   node oc-stats.mjs --top N            # 只显示 token 用量前 N 的模型
//   OPENCODE2API_INSTANCES='[{...}]' node oc-stats.mjs
//
// 环境变量：
//   OPENCODE2API_STATS       stats 端点（默认 http://127.0.0.1:8080/v1/stats）
//   OPENCODE2API_LOCAL_KEY   本地 API key
//   OPENCODE2API_INSTANCES   多实例 JSON 数组，格式同 dashboard.mjs

import { readFileSync, existsSync } from "node:fs";
import { join } from "node:path";

function loadEnv() {
  const env = {};
  const p = join(process.cwd(), ".env");
  if (p && existsSync(p)) {
    for (const line of readFileSync(p, "utf8").split(/\r?\n/)) {
      const m = line.match(/^\s*([A-Z0-9_]+)\s*=\s*(.*)\s*$/);
      if (m) env[m[1]] = m[2];
    }
  }
  return env;
}
const fileEnv = loadEnv();

const args = process.argv.slice(2);
const watch = args.includes("--watch");
const asJson = args.includes("--json");
const topIdx = args.indexOf("--top");
const topN = topIdx >= 0 && args[topIdx + 1] ? Number(args[topIdx + 1]) : null;

// Resolve instances.
let instances = [];
try {
  const raw = process.env.OPENCODE2API_INSTANCES || fileEnv.OPENCODE2API_INSTANCES;
  if (raw) instances = JSON.parse(raw);
} catch { instances = []; }
if (instances.length === 0) {
  instances = [{
    name: "default",
    stats: process.env.OPENCODE2API_STATS || "http://127.0.0.1:8080/v1/stats",
    key: process.env.OPENCODE2API_LOCAL_KEY || fileEnv.OPENCODE2API_LOCAL_KEY || "",
  }];
}

async function fetchStat(inst) {
  try {
    const headers = {};
    if (inst.key) headers["Authorization"] = `Bearer ${inst.key}`;
    const r = await fetch(inst.stats, { headers });
    if (!r.ok) return { name: inst.name, error: `HTTP ${r.status}` };
    const data = await r.json();
    return { name: inst.name, stats: data };
  } catch (e) {
    return { name: inst.name, error: String(e.message || e) };
  }
}

function merge(results) {
  const total = { requests: 0, success: 0, failed: 0, input_tokens: 0, output_tokens: 0, cached_tokens: 0, reasoning_tokens: 0, cost: 0 };
  const models = new Map();
  const errors = [];
  for (const r of results) {
    if (r.error || !r.stats) { errors.push(`${r.name}: ${r.error}`); continue; }
    const t = r.stats.total || {};
    for (const k of Object.keys(total)) total[k] += t[k] || 0;
    for (const m of r.stats.models || []) {
      const cur = models.get(m.model) || { model: m.model, stats: { requests: 0, input_tokens: 0, output_tokens: 0, cached_tokens: 0, reasoning_tokens: 0, cost: 0 } };
      for (const k of Object.keys(cur.stats)) cur.stats[k] += (m.stats?.[k] || 0);
      models.set(m.model, cur);
    }
  }
  return { total, models: [...models.values()], errors };
}

function pad(s, w, right) {
  s = String(s);
  return s.length >= w ? s : right ? s.padEnd(w) : s.padStart(w);
}
function fmtCost(n) { return n ? `$${Number(n).toFixed(4)}` : "$0.0000"; }
function fmtTokens(n) {
  n = Number(n) || 0;
  if (n >= 1e9) return (n / 1e9).toFixed(2) + "B";
  if (n >= 1e6) return (n / 1e6).toFixed(2) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(1) + "K";
  return String(n);
}

function render(m) {
  const t = m.total;
  const okRate = t.requests ? Math.round((t.success / t.requests) * 100) : 0;
  const lines = [];
  lines.push("=== opencode2api 用量 ===");
  lines.push(pad("请求", 10, true) + pad("成功", 8, true) + pad("失败", 8, true) + pad("成功率", 8, true) + pad("Input", 12, true) + pad("Output", 12, true) + pad("Reason", 12, true) + pad("成本", 12, true));
  lines.push(pad(t.requests, 10, true) + pad(t.success, 8, true) + pad(t.failed, 8, true) + pad(okRate + "%", 8, true) + pad(fmtTokens(t.input_tokens), 12, true) + pad(fmtTokens(t.output_tokens), 12, true) + pad(fmtTokens(t.reasoning_tokens), 12, true) + pad(fmtCost(t.cost), 12, true));
  if (m.errors.length) {
    lines.push("\n[警告] 以下实例不可达:");
    for (const e of m.errors) lines.push("  - " + e);
  }
  let rows = m.models.sort((a, b) => (b.stats.input_tokens + b.stats.output_tokens) - (a.stats.input_tokens + a.stats.output_tokens));
  if (topN) rows = rows.slice(0, topN);
  if (rows.length) {
    lines.push("\n按模型排序 (token 用量):");
    lines.push(pad("模型", 30, false) + pad("请求", 8, true) + pad("Input", 12, true) + pad("Output", 12, true) + pad("成本", 12, true));
    for (const r of rows) {
      lines.push(pad(r.model, 30, false) + pad(r.stats.requests, 8, true) + pad(fmtTokens(r.stats.input_tokens), 12, true) + pad(fmtTokens(r.stats.output_tokens), 12, true) + pad(fmtCost(r.stats.cost), 12, true));
    }
  }
  return lines.join("\n");
}

async function run() {
  if (asJson) {
    const results = await Promise.all(instances.map(fetchStat));
    const m = merge(results);
    console.log(JSON.stringify({ merged: { total: m.total, models: m.models }, errors: m.errors }, null, 2));
    return;
  }
  const results = await Promise.all(instances.map(fetchStat));
  const m = merge(results);
  const out = render(m);
  if (watch) {
    console.clear();
    console.log(new Date().toLocaleTimeString());
  }
  console.log(out);
  if (watch) {
    setTimeout(run, 5000);
  }
}

run().catch((e) => { console.error(e); process.exit(1); });