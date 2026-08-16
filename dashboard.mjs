import { createServer } from "node:http";
import { readFileSync, statSync, existsSync, writeFileSync, copyFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { homedir } from "node:os";
import { execFile } from "node:child_process";
import { spawn } from "node:child_process";

// ---- config management ----------------------------------------------------
// Targets:
//   gateway         -> <dir>/config.json            (opencode2api gateway)
//   opencode-global -> ~/.config/opencode/opencode.json
//   opencode-user   -> <dir>/opencode.json          (project-level, next to gateway)
const CONFIG_TARGETS = {
  "gateway": {
    label: "网关配置 (config.json)",
    path: () => join(import.meta.dirname, "config.json"),
    restartOnSave: true,
  },
  "opencode-global": {
    label: "opencode 全局配置 (~/.config/opencode/opencode.json)",
    path: () => join(homedir(), ".config", "opencode", "opencode.json"),
    restartOnSave: false,
  },
  "opencode-user": {
    label: "opencode 用户配置 (项目 opencode.json)",
    path: () => join(import.meta.dirname, "opencode.json"),
    restartOnSave: false,
  },
};

function maskKey(key) {
  if (!key || key.length <= 8) return "********";
  return key.slice(0, 4) + "****" + key.slice(-4);
}

function unmaskKeys(list, current) {
  return (list || []).map((k, i) => {
    if (typeof k !== "string") return k;
    if (k.includes("****") && current && current[i]) return current[i];
    return k;
  });
}

function redactConfig(cfg) {
  const out = JSON.parse(JSON.stringify(cfg || {}));
  for (const field of ["server_keys", "zen_keys", "go_keys"]) {
    if (Array.isArray(out[field])) out[field] = out[field].map(maskKey);
  }
  if (out.provider && out.provider.zen2api && out.provider.zen2api.options) {
    const opt = out.provider.zen2api.options;
    if (opt.apiKey) opt.apiKey = maskKey(opt.apiKey);
  }
  return out;
}

function readTarget(target) {
  const spec = CONFIG_TARGETS[target];
  if (!spec) return { error: "unknown target" };
  const path = spec.path();
  if (!existsSync(path)) return { error: "not_found", path, label: spec.label };
  let cfg;
  try {
    cfg = JSON.parse(readFileSync(path, "utf8"));
  } catch (e) {
    return { error: "parse_error", detail: String(e.message), path, label: spec.label };
  }
  return { ok: true, path, label: spec.label, target, config: redactConfig(cfg) };
}

async function writeTarget(target, rawConfig) {
  const spec = CONFIG_TARGETS[target];
  if (!spec) return { error: "unknown target" };
  const path = spec.path();
  let newCfg;
  try {
    newCfg = JSON.parse(rawConfig);
  } catch (e) {
    return { error: "parse_error", detail: String(e.message) };
  }
  let current = {};
  if (existsSync(path)) {
    try { current = JSON.parse(readFileSync(path, "utf8") || "{}"); } catch { current = {}; }
  } else if (!newCfg.provider) {
    // Brand-new opencode config: seed a minimal valid structure.
    newCfg = {
      "$schema": "https://opencode.ai/config.json",
      model: "zen2api/deepseek-v4-flash-free",
      provider: { zen2api: { npm: "@ai-sdk/openai-compatible", name: "Zen Local (opencode2api)", options: { baseURL: "http://127.0.0.1:8080/v1", apiKey: "" }, models: {} } },
    };
  }

  // Restore real keys when the client sent masked placeholders.
  for (const field of ["server_keys", "zen_keys", "go_keys"]) {
    if (Array.isArray(newCfg[field]) && Array.isArray(current[field])) {
      newCfg[field] = unmaskKeys(newCfg[field], current[field]);
    }
  }
  if (newCfg.provider && newCfg.provider.zen2api && newCfg.provider.zen2api.options && current.provider?.zen2api?.options) {
    const apiKey = newCfg.provider.zen2api.options.apiKey;
    if (typeof apiKey === "string" && apiKey.includes("****")) {
      newCfg.provider.zen2api.options.apiKey = current.provider.zen2api.options.apiKey;
    }
  }

  // Backup before overwriting.
  const backup = path + ".bak";
  try { copyFileSync(path, backup); } catch {}

  try {
    writeFileSync(path, JSON.stringify(newCfg, null, 2) + "\n", "utf8");
  } catch (e) {
    return { error: "write_error", detail: String(e.message) };
  }

  // Validate gateway config by asking the running gateway to parse it is not
  // possible without a reload; the restart below will surface problems.
  return { ok: true, path, target, backup, restarted: false };
}

function sleep(ms) { return new Promise((r) => setTimeout(r, ms)); }

// Restart the gateway process (Windows). Kills opencode2api.exe, then spawns
// a fresh detached instance from the gateway directory.
async function restartGateway() {
  const exe = join(import.meta.dirname, "opencode2api.exe");
  if (!existsSync(exe)) return { error: "gateway exe not found" };
  const results = [];
  await new Promise((resolve) => {
    execFile("taskkill", ["/IM", "opencode2api.exe", "/F"], { windowsHide: true }, (err, _stdout, stderr) => {
      results.push(err ? `kill: ${(stderr || err.message || "").trim() || "no process"}` : "kill ok");
      resolve();
    });
  });
  await sleep(1200);
  const logOut = join(import.meta.dirname, "opencode2api.log");
  const logErr = join(import.meta.dirname, "opencode2api.err.log");
  const child = spawn(exe, [], {
    cwd: import.meta.dirname,
    detached: true,
    stdio: "ignore",
    windowsHide: true,
  });
  child.unref();
  results.push("spawned");
  return { ok: true, detail: results.join("; "), log: logOut, err: logErr };
}

// Test a server key against the local gateway /v1/models endpoint.
async function testKey(key) {
  const inst = instances[0] || {};
  const base = inst.health ? inst.health.replace(/\/healthz$/, "") : "http://127.0.0.1:8080";
  try {
    const r = await fetch(`${base}/v1/models`, { headers: { Authorization: `Bearer ${key}` }, signal: AbortSignal.timeout(5000) });
    return { ok: r.ok, status: r.status };
  } catch (e) {
    return { ok: false, status: 0, detail: String(e.message || e) };
  }
}

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

async function handleApiModels(res) {
  // Real-time model list straight from the gateway /v1/models (no caching).
  const inst = instances[0] || {};
  const base = inst.health ? inst.health.replace(/\/healthz$/, "") : "http://127.0.0.1:8080";
  const url = `${base}/v1/models`;
  try {
    const headers = {};
    if (inst.key) headers["Authorization"] = `Bearer ${inst.key}`;
    const r = await fetch(url, { headers, signal: AbortSignal.timeout(5000) });
    const data = await r.json();
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ ok: r.ok, status: r.status, models: (data.data || []).map((m) => m.id), fetched: Date.now() }));
  } catch (e) {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ ok: false, error: String(e.message || e), fetched: Date.now() }));
  }
}

async function handleApiConfig(req, res, url) {
  const target = url.searchParams.get("target") || "gateway";
  const result = readTarget(target);
  res.writeHead(200, { "Content-Type": "application/json" });
  res.end(JSON.stringify({ ...result, fetched: Date.now() }));
}

async function handleApiSaveConfig(req, res, url) {
  const target = url.searchParams.get("target") || "gateway";
  const chunks = [];
  for await (const c of req) chunks.push(c);
  const body = Buffer.concat(chunks).toString("utf8");
  const result = await writeTarget(target, body);
  if (result.ok) {
    const spec = CONFIG_TARGETS[target];
    if (spec && spec.restartOnSave) {
      const rst = await restartGateway();
      result.restarted = rst.ok;
      result.restartDetail = rst.detail || rst.error;
    }
  }
  res.writeHead(200, { "Content-Type": "application/json" });
  res.end(JSON.stringify({ ...result, fetched: Date.now() }));
}

async function handleApiRestart(req, res) {
  const result = await restartGateway();
  res.writeHead(200, { "Content-Type": "application/json" });
  res.end(JSON.stringify({ ...result, fetched: Date.now() }));
}

async function handleApiTestKey(req, res) {
  const chunks = [];
  for await (const c of req) chunks.push(c);
  let key = "";
  try {
    key = JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}").key || "";
  } catch {}
  const result = key ? await testKey(key) : { ok: false, status: 0, detail: "no key provided" };
  res.writeHead(200, { "Content-Type": "application/json" });
  res.end(JSON.stringify({ ...result, fetched: Date.now() }));
}

async function handleApiAuditRecent(res, url) {
  const limit = Math.min(Number(url.searchParams.get("limit") || 50), 500);
  const history = readAuditHistory();
  const entries = history.entries.slice(-limit).reverse();
  res.writeHead(200, { "Content-Type": "application/json" });
  res.end(JSON.stringify({ entries, total_lines: history.total_lines, fetched: Date.now() }));
}

// ---- server ---------------------------------------------------------------
const html = readFileSync(join(import.meta.dirname, "dashboard.html"), "utf8")
  .replaceAll("window.__COST_LIMIT__ || 0", `window.__COST_LIMIT__ || ${COST_LIMIT}`);

createServer(async (req, res) => {
  const url = new URL(req.url, `http://${req.headers.host || "localhost"}`);
  const path = url.pathname;
  if (path === "/" || path === "/index.html") {
    res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
    res.end(html);
  } else if (path === "/api/stats") {
    await handleApiStats(res);
  } else if (path === "/api/gateway-stats") {
    await handleApiGatewayStats(res);
  } else if (path === "/api/audit") {
    await handleApiAudit(res);
  } else if (path === "/api/audit-recent") {
    await handleApiAuditRecent(res, url);
  } else if (path === "/api/health") {
    await handleApiHealth(res);
  } else if (path === "/api/models") {
    await handleApiModels(res);
  } else if (path === "/api/config") {
    if (req.method === "POST") await handleApiSaveConfig(req, res, url);
    else await handleApiConfig(req, res, url);
  } else if (path === "/api/restart") {
    await handleApiRestart(req, res);
  } else if (path === "/api/test-key") {
    await handleApiTestKey(req, res);
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
