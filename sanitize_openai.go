package main

// geminiUnsupportedParams lists OpenAI chat-completion request fields that
// Gemini's OpenAI-compatible endpoint (generativelanguage.googleapis.com/
// v1beta/openai) rejects with an opaque 400 "Bad Request". The gateway strips
// them so clients (e.g. the opencode ai-sdk) can send a full OpenAI-shaped
// request without tripping upstream validation.
var geminiUnsupportedParams = []string{
	"logprobs",
	"top_logprobs",
	"logit_bias",
	"seed",
	"echo",
	"best_of",
	"frequency_penalty",
	"presence_penalty",
	"repetition_penalty",
	"function_call",
	"functions",
	"metadata",
	"reasoning_effort",
	"service_tier",
}

// sanitizeOpenAIBody removes request fields the upstream OpenAI-compatible
// endpoint rejects, and renames/normalizes a few that have different names or
// ranges upstream. It only deletes or normalizes keys the client actually sent
// — it never injects new fields, because Gemini also rejects explicit null
// values with the same 400.
func sanitizeOpenAIBody(payload map[string]any) map[string]any {
	for _, key := range geminiUnsupportedParams {
		delete(payload, key)
	}

	// Gemini names the output cap "max_tokens"; the newer OpenAI param
	// "max_completion_tokens" makes Gemini return 400 "Bad Request". Rename
	// it (only when the client did not also send max_tokens).
	if _, hasMax := payload["max_tokens"]; !hasMax {
		if v, ok := payload["max_completion_tokens"]; ok {
			delete(payload, "max_completion_tokens")
			payload["max_tokens"] = v
		}
	} else {
		delete(payload, "max_completion_tokens")
	}

	// Gemini only supports a single completion (n=1).
	if n, ok := payload["n"]; ok {
		if f, ok := toFloat(n); ok && f > 1 {
			payload["n"] = 1
		} else if i, ok := n.(int); ok && i > 1 {
			payload["n"] = 1
		}
	}

	// Gemini caps top_p at [0, 1]; clamp instead of dropping so valid values pass.
	if v, ok := payload["top_p"]; ok {
		if f, ok := toFloat(v); ok {
			switch {
			case f < 0:
				payload["top_p"] = 0.0
			case f > 1:
				payload["top_p"] = 1.0
			}
		}
	}

	// Gemini temperature range is [0, 2]; clamp out-of-range instead of dropping.
	if v, ok := payload["temperature"]; ok {
		if f, ok := toFloat(v); ok {
			switch {
			case f < 0:
				payload["temperature"] = 0.0
			case f > 2:
				payload["temperature"] = 2.0
			}
		}
	}

	// Gemini's OpenAI endpoint rejects a top-level "system" field; fold it into
	// the messages array as a system role so the system prompt is preserved.
	if sys, ok := payload["system"]; ok {
		if s, ok := sys.(string); ok && s != "" {
			if msgs, ok := payload["messages"].([]any); ok {
				payload["messages"] = append([]any{map[string]any{"role": "system", "content": s}}, msgs...)
			}
		}
		delete(payload, "system")
	}

	return payload
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}
