import { createServer } from "node:http";
import { readFileSync, statSync, existsSync } from "node:fs";
import { join } from "node:path";

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
const HEALTH = process.env.OPENCODE2API_HEALTH || "http://127.0.0.1:8080/healthz";
const STATS = process.env.OPENCODE2API_STATS || "http://127.0.0.1:8080/v1/stats";
const GATEWAY_KEY = process.env.OPENCODE2API_LOCAL_KEY || fileEnv.OPENCODE2API_LOCAL_KEY;
const PORT = Number(process.env.OPENCODE2API_DASHBOARD_PORT || fileEnv.OPENCODE2API_DASHBOARD_PORT || 9090);

if (!GATEWAY_KEY) {
  console.error("OPENCODE2API_LOCAL_KEY is required (env or .env file) to read /v1/stats");
}

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

async function handleApiStats(res) {
  const raw = readFileSync(LOG, "utf8");
  const entries = raw.split(/\r?\n/).filter(Boolean).map(parseLine);
  const stats = analyze(entries);
  const buckets = minutesBuckets(entries);
  res.writeHead(200, { "Content-Type": "application/json" });
  res.end(JSON.stringify({ ...stats, buckets, total: entries.length, updated: Date.now() }));
}

async function handleApiGatewayStats(res) {
  try {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), 4000);
    const r = await fetch(STATS, {
      signal: ctrl.signal,
      headers: { "Authorization": `Bearer ${GATEWAY_KEY}` },
    });
    clearTimeout(timer);
    const data = await r.json();
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ ...data, fetched: Date.now() }));
  } catch (e) {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: "gateway_stats_unreachable", detail: String(e.message || e), fetched: Date.now() }));
  }
}

async function handleApiHealth(res) {
  try {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), 3000);
    const r = await fetch(HEALTH, { signal: ctrl.signal });
    clearTimeout(timer);
    const data = await r.json();
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ ...data, fetched: Date.now() }));
  } catch {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: "gateway_unreachable", fetched: Date.now() }));
  }
}

const html = readFileSync(join(import.meta.dirname, "dashboard.html"), "utf8");

createServer(async (req, res) => {
  if (req.url === "/" || req.url === "/index.html") {
    res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
    res.end(html);
  } else if (req.url === "/api/stats") {
    handleApiStats(res);
  } else if (req.url === "/api/gateway-stats") {
    await handleApiGatewayStats(res);
  } else if (req.url === "/api/health") {
    await handleApiHealth(res);
  } else {
    res.writeHead(404);
    res.end("not found");
  }
}).listen(PORT, () => {
  console.log(`OpenCode2API dashboard: http://127.0.0.1:${PORT}`);
});
