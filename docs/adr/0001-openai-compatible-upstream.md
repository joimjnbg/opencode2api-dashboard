# ADR-0001: Support an OpenAI-compatible upstream (Gemini) alongside opencode

## Status

Accepted (2026-08-16)

## Context

The gateway was built to pool free opencode.ai accounts. opencode.ai requires a
payment method on file even for free-tier models, so accounts without one are
rejected with `401 No payment method` and no amount of key rotation helps.

The user wants genuinely free, no-card usage. Google AI Studio (Gemini) offers a
free tier with an API key and **no payment method**. Gemini exposes an
OpenAI-compatible surface, so chat completions are already a passthrough — but
the gateway was coupled to opencode-specific behaviour:

- opencode client headers (`x-opencode-*`, `x-machine-id`);
- a `supportedModel` blocklist that rejected `gemini-*`;
- a model catalog fetched from opencode's `/v1/models`;
- quota patterns written for opencode's error strings;
- path prefixes (`/v1/chat/completions`) that don't match Gemini's base URL.

## Decision

Introduce `upstream_mode` (`opencode` default, `openai` for Gemini). Keep
opencode behaviour bit-for-bit identical when mode is `opencode`. In `openai`
mode:

- send only standard headers (no opencode headers), neutral User-Agent;
- allow `gemini-*` models;
- load the catalog from `models.static` (Gemini's `/models` is not OpenAI-shaped);
- map chat to `<base>/chat/completions`;
- treat a bare 429 as a per-key throttle (not quota parking) so rate limits
  rotate to the next key; daily-quota bodies still trigger quota cooldown via
  OpenAI-compatible patterns.

The pooling logic (fair rotation, per-key throttle, quota parking) is untouched
and now serves Gemini equally.

## Consequences

- The gateway can front any OpenAI-compatible free provider, not just opencode.
- Gemini's free-tier RPM/TPM limits are now spread across multiple keys.
- `config.json` is mode-specific; `upstream_mode: "openai"` requires
  `upstream.zen` to point at the OpenAI-compatible base URL and `models.static`
  to list available models.
