import { createServer } from "node:http";
import { readFileSync, statSync, existsSync } from "node:fs";
import { join } from "node:path";

// ---- env helpers ----------------------------------------------------------
function loadEnv() {
  const env = {};
  const p = join(import.meta.dirname, ".env");
  if (existsSync(p)) {
    for (const line of readFileSync(p, "utf8").split(/\r?\n/)) {
      const m = line.match(/^\s*([A-Z0-9_]+)\s*=\s*(.*)\s*$/);
      if (m) env[m[1]] = m[2];
    }
  }
  return env;
}

const fileEnv = loadEnv();
const LOG = process.env.OPENCODE2API_LOG || join(import.meta.dirname, "opencode2api.log");
const AUDIT = process.env.OPENCODE2API_AUDIT || join(import.meta.dirname, "opencode2api.audit.jsonl");
const PORT = Number(process.env.OPENCODE2API_DASHBOARD_PORT || fileEnv.OPENCODE2API_DASHBOARD_PORT || 9090);
const ALERT_WEBHOOK = process.env.OPENCODE2API_ALERT_WEBHOOK || fileEnv.OPENCODE2API_ALERT_WEBHOOK || "";
const ALERT_INTERVAL = Number(process.env.OPENCODE2API_ALERT_INTERVAL || fileEnv.OPENCODE2API_ALERT_INTERVAL || 30);
const COST_LIMIT = Number(process.env.OPENCODE2API_ALERT_COST || fileEnv.OPENCODE2API_ALERT_COST || 0);
const FAILURE_RATE_LIMIT = Number(process.env.OPENCODE2API_ALERT_FAILURE_RATE || fileEnv.OPENCODE2API_ALERT_FAILURE_RATE || 50);

// Multi-instance aggregation: OPENCODE2API_INSTANCES is a JSON array like
//   [{"name":"home","health":"http://127.0.0.1:8080/healthz","stats":"http://127.0.0.1:8080/v1/stats","key":"sk-..."}]
// When absent, a single instance is derived from the standard endpoints.
let instances = [];
try {
  const raw = process.env.OPENCODE2API_INSTANCES || fileEnv.OPENCODE2API_INSTANCES;
  if (raw) instances = JSON.parse(raw);
} catch { instances = []; }
if (instances.length === 0) {
  const key = process.env.OPENCODE2API_LOCAL_KEY || fileEnv.OPENCODE2API_LOCAL_KEY;
  instances = [{
    name: "default",
    health: process.env.OPENCODE2API_HEALTH || "http://127.0.0.1:8080/healthz",
    stats: process.env.OPENCODE2API_STATS || "http://127.0.0.1:8080/v1/stats",
    key,
  }];
}

// ---- gateway fetch --------------------------------------------------------
async function fetchJson(url, key, timeoutMs = 4000) {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), timeoutMs);
  try {
    const headers = {};
    if (key) headers["Authorization"] = `Bearer ${key}`;
    const r = await fetch(url, { signal: ctrl.signal, headers });
    const data = await r.json();
    return data;
  } catch (e) {
    return { error: "unreachable", detail: String(e.message || e) };
  } finally {
    clearTimeout(timer);
  }
}

async function fetchAllInstances() {
  return await Promise.all(instances.map(async (inst) => {
    const [health, stats] = await Promise.all([
      fetchJson(inst.health, null, 3000),
      fetchJson(inst.stats, inst.key, 4000),
    ]);
    return { name: inst.name, health, stats, fetched: Date.now() };
  }));
}

// ---- aggregation ----------------------------------------------------------
function mergeStats(results) {
  const totals = { requests: 0, success: 0, failed: 0, input_tokens: 0, output_tokens: 0, cached_tokens: 0, reasoning_tokens: 0, cost: 0 };
  const byModel = new Map();
  const hourMap = new Map();
  let anyOk = false;

  for (const r of results) {
    const s = r.stats;
    if (!s || s.error) continue;
    anyOk = true;
    const t = s.total || {};
    totals.requests += t.requests || 0;
    totals.success += t.success || 0;
    totals.failed += t.failed || 0;
    totals.input_tokens += t.input_tokens || 0;
    totals.output_tokens += t.output_tokens || 0;
    totals.cached_tokens += t.cached_tokens || 0;
    totals.reasoning_tokens += t.reasoning_tokens || 0;
    totals.cost += t.cost || 0;

    for (const m of s.models || []) {
      const cur = byModel.get(m.model) || { model: m.model, stats: { requests: 0, success: 0, failed: 0, input_tokens: 0, output_tokens: 0, cached_tokens: 0, reasoning_tokens: 0, cost: 0 } };
      for (const k of ["requests","success","failed","input_tokens","output_tokens","cached_tokens","reasoning_tokens"]) cur.stats[k] += m.stats?.[k] || 0;
      cur.stats.cost += m.stats?.cost || 0;
      byModel.set(m.model, cur);
    }
    for (const h of s.hours || []) {
      const cur = hourMap.get(h.hour) || { hour: h.hour, stats: { requests: 0, success: 0, failed: 0, input_tokens: 0, output_tokens: 0, cached_tokens: 0, reasoning_tokens: 0, cost: 0 } };
      for (const k of ["requests","success","failed","input_tokens","output_tokens","cached_tokens","reasoning_tokens"]) cur.stats[k] += h.stats?.[k] || 0;
      cur.stats.cost += h.stats?.cost || 0;
      hourMap.set(h.hour, cur);
    }
  }
  if (!anyOk) return { error: "all_instances_unreachable" };
  return {
    uptime_seconds: 0,
    total: totals,
    models: [...byModel.values()],
    hours: [...hourMap.values()],
    instances: results,
  };
}

// ---- audit history --------------------------------------------------------
function readAuditHistory(maxLines = 50000) {
  if (!existsSync(AUDIT)) return { entries: [], total_lines: 0 };
  const raw = readFileSync(AUDIT, "utf8");
  const lines = raw.split(/\r?\n/).filter(Boolean);
  const entries = lines.slice(-maxLines).map((line) => {
    try { return JSON.parse(line); } catch { return null; }
  }).filter(Boolean);
  return { entries, total_lines: lines.length };
}

function historyByDay(entries) {
  const map = new Map();
  for (const e of entries) {
    const day = (e.ts || "").slice(0, 10);
    if (!day) continue;
    const cur = map.get(day) || { day, requests: 0, input_tokens: 0, output_tokens: 0, cost: 0 };
    cur.requests++;
    cur.input_tokens += e.usage?.Input || 0;
    cur.output_tokens += e.usage?.Output || 0;
    cur.cost += e.cost || 0;
    map.set(day, cur);
  }
  return [...map.values()].sort((a, b) => a.day.localeCompare(b.day));
}

// ---- alerting -------------------------------------------------------------
let alertState = {};

function fmtCost(n) { return "$" + Number(n || 0).toFixed(4); }

async function sendAlert(title, body) {
  if (!ALERT_WEBHOOK) return;
  try {
    const payload = ALERT_WEBHOOK.includes("api.telegram.org")
      ? { text: `${title}\n${body}`, chat_id: null }
      : { text: `${title}\n${body}` };
    // Telegram expects sendMessage with text field; generic webhooks get a flat object.
    await fetch(ALERT_WEBHOOK, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(ALERT_WEBHOOK.includes("api.telegram.org")
        ? { text: `${title}\n${body}` }
        : payload),
    });
  } catch (e) {
    console.error("alert send failed:", e.message);
  }
}

async function checkAlerts() {
  const results = await fetchAllInstances();
  const merged = mergeStats(results);

  // Gateway unreachable check.
  for (const r of results) {
    const key = r.name;
    const healthy = r.health && !r.health.error && r.health.status;
    const prev = alertState[key];
    if (!healthy && !(prev && prev.down)) {
      alertState[key] = { down: true };
      await sendAlert(`[opencode2api] 网关 ${r.name} 不可达`, `Health: ${JSON.stringify(r.health).slice(0, 200)}`);
    } else if (healthy && prev && prev.down) {
      alertState[key] = { down: false };
      await sendAlert(`[opencode2api] 网关 ${r.name} 已恢复`, "Health check passed again.");
    }
    if (healthy) alertState[key] = { down: false };
  }

  if (merged.error || !merged.total) return;

  // Cost limit check.
  if (COST_LIMIT > 0) {
    const cost = merged.total.cost || 0;
    if (cost >= COST_LIMIT && !alertState.costFired) {
      alertState.costFired = true;
      await sendAlert(`[opencode2api] 成本超阈值`, `累计成本 ${fmtCost(cost)} >= ${fmtCost(COST_LIMIT)}`);
    } else if (cost < COST_LIMIT * 0.8) {
      alertState.costFired = false;
    }
  }

  // Failure rate check.
  if (FAILURE_RATE_LIMIT > 0) {
    const t = merged.total;
    const rate = t.requests ? (t.failed / t.requests) * 100 : 0;
    if (rate >= FAILURE_RATE_LIMIT && !alertState.rateFired) {
      alertState.rateFired = true;
      await sendAlert(`[opencode2api] 失败率过高`, `失败率 ${rate.toFixed(1)}%（${t.failed}/${t.requests}）>= ${FAILURE_RATE_LIMIT}%`);
    } else if (rate < FAILURE_RATE_LIMIT * 0.5) {
      alertState.rateFired = false;
    }
  }
}

// ---- log parsing ----------------------------------------------------------
function parseLine(line) {
  const m = line.match(/time=([\dT:.\-]+)/);
  const level = line.match(/level=(\w+)/);
  const msg = line.match(/msg="([^"]+)"/);
  const reqId = line.match(/request_id=(\S+)/);
  const attempt = line.match(/attempt=(\d+)/);
  const tier = line.match(/tier=(\w+)/);
  const proxy = line.match(/proxy=(\S+)/);
  const status = line.match(/status=(\d+)/);
  return {
    time: m ? m[1] : "",
    level: level ? level[1] : "",
    msg: msg ? msg[1] : "",
    request_id: reqId ? reqId[1] : "",
    attempt: attempt ? attempt[1] : "",
    tier: tier ? tier[1] : "",
    proxy: proxy ? proxy[1] : "",
    status: status ? status[1] : "",
  };
}

function analyze(entries) {
  const accepted = entries.filter((e) => e.msg === "upstream accepted request");
  const rejected = entries.filter((e) => e.msg === "upstream rejected request");
  const failed = entries.filter((e) => e.msg === "upstream request failed");
  const rejectedNoRetry = entries.filter((e) => e.msg === "upstream rejected request without retry");
  const byStatus = {};
  for (const e of rejected) {
    const s = e.status || "?";
    byStatus[s] = (byStatus[s] || 0) + 1;
  }
  const recent = entries.slice(-30).reverse();
  return { accepted: accepted.length, rejected: rejected.length, rejectedNoRetry: rejectedNoRetry.length, failed: failed.length, byStatus, recent };
}

function minutesBuckets(entries, buckets = 30) {
  const now = Date.now();
  const out = new Array(buckets).fill(0);
  for (const e of entries) {
    const t = new Date(e.time).getTime();
    if (!t) continue;
    const idx = Math.max(0, buckets - 1 - Math.floor((now - t) / 60000));
    if (idx >= 0 && idx < buckets) out[idx]++;
  }
  return out;
}

// ---- handlers -------------------------------------------------------------
async function handleApiStats(res) {
  let raw = "";
  try { raw = readFileSync(LOG, "utf8"); } catch {}
  const entries = raw.split(/\r?\n/).filter(Boolean).map(parseLine);
  const stats = analyze(entries);
  const buckets = minutesBuckets(entries);
  res.writeHead(200, { "Content-Type": "application/json" });
  res.end(JSON.stringify({ ...stats, buckets, total: entries.length, updated: Date.now() }));
}

async function handleApiGatewayStats(res) {
  const results = await fetchAllInstances();
  const merged = mergeStats(results);
  res.writeHead(200, { "Content-Type": "application/json" });
  res.end(JSON.stringify({ ...merged, fetched: Date.now() }));
}

async function handleApiAudit(res) {
  const history = readAuditHistory();
  const byDay = historyByDay(history.entries);
  res.writeHead(200, { "Content-Type": "application/json" });
  res.end(JSON.stringify({ by_day: byDay, total_lines: history.total_lines, fetched: Date.now() }));
}

async function handleApiHealth(res) {
  const results = await fetchAllInstances();
  res.writeHead(200, { "Content-Type": "application/json" });
  res.end(JSON.stringify({ instances: results, fetched: Date.now() }));
}

// ---- server ---------------------------------------------------------------
const html = readFileSync(join(import.meta.dirname, "dashboard.html"), "utf8")
  .replaceAll("window.__COST_LIMIT__ || 0", `window.__COST_LIMIT__ || ${COST_LIMIT}`);

createServer(async (req, res) => {
  if (req.url === "/" || req.url === "/index.html") {
    res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
    res.end(html);
  } else if (req.url === "/api/stats") {
    await handleApiStats(res);
  } else if (req.url === "/api/gateway-stats") {
    await handleApiGatewayStats(res);
  } else if (req.url === "/api/audit") {
    await handleApiAudit(res);
  } else if (req.url === "/api/health") {
    await handleApiHealth(res);
  } else {
    res.writeHead(404);
    res.end("not found");
  }
}).listen(PORT, () => {
  console.log(`OpenCode2API dashboard: http://127.0.0.1:${PORT}`);
  if (ALERT_WEBHOOK) {
    setInterval(checkAlerts, ALERT_INTERVAL * 1000);
    console.log(`Alerts enabled -> ${ALERT_WEBHOOK.split("/").slice(0,3).join("/")}... interval ${ALERT_INTERVAL}s`);
  }
});
