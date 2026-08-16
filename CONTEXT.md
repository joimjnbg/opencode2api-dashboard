# CONTEXT.md

`opencode2api` is a local API gateway that fronts one or more upstream LLM
providers behind a single OpenAI-compatible endpoint
(`http://127.0.0.1:8080/v1/chat/completions`). Its job is to let a client use
**several free-tier provider keys as one pool** with fair usage, so no single
key burns out first and the whole pool survives provider rate limits.

## Why it exists

Free LLM tiers (opencode.ai, Google AI Studio / Gemini, …) cap usage per key
and per minute. A single key throttles fast. By pooling multiple keys and
rotating requests fairly, the gateway stretches free quota further and hides
provider 429 / quota errors from the client through silent failover,
backpressure, and quota parking.

## Domain terms

- **上游 (upstream)** — the provider the gateway forwards to. Two shapes today:
  `opencode` (opencode.ai, opencode client headers) and `openai` (any
  OpenAI-compatible endpoint such as Gemini at
  `https://generativelanguage.googleapis.com/v1beta/openai`). Selected by
  `upstream_mode`.
- **key / 账号 (account)** — one provider credential. In `multi_account` mode
  each key is treated as its own account: a 429 throttles only that key, never
  the whole pool.
- **池 (pool)** — the set of keys for one tier; the unit of fair rotation.
- **公平轮转 (fair rotation)** — requests spread round-robin (or by fair order)
  across healthy keys instead of pinning a conversation to one key.
- **节流 (throttle)** — a key enters a cooldown window after 429s, backing off
  with exponential growth; the request waits (backpressure) instead of failing.
- **额度停用 (quota parking)** — when a key hits free-tier quota exhaustion it
  is parked out of rotation for the quota window, then rejoins automatically.
- **代理 (proxy)** — egress path (direct or SOCKS5); health-checked separately
  from key state.

## Request shape

Clients speak OpenAI chat completions. `opencode` upstream also exposes
`/v1/responses` and `/v1/messages`; `openai` upstream only exposes
`/chat/completions`. The model catalog is fetched from the upstream (opencode)
or taken from `models.static` (openai).

## Layout

See `docs/adr/` for recorded decisions. Skills: read this file first, then the
relevant ADR before changing pool / failover / upstream code.
