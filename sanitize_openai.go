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
}

// sanitizeOpenAIBody removes request fields the upstream OpenAI-compatible
// endpoint rejects. It only deletes or normalizes keys the client actually
// sent — it never injects new fields, because Gemini also rejects explicit
// null values with the same 400.
func sanitizeOpenAIBody(payload map[string]any) map[string]any {
	for _, key := range geminiUnsupportedParams {
		delete(payload, key)
	}

	// Gemini only supports a single completion (n=1).
	if n, ok := payload["n"]; ok {
		if f, ok := toFloat(n); ok && f > 1 {
			payload["n"] = 1
		} else if i, ok := n.(int); ok && i > 1 {
			payload["n"] = 1
		}
	}

	// Gemini caps top_p at 1.0; clamp instead of dropping so valid values pass.
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
