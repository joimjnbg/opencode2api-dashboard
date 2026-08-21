package main

// sanitizeOpenAIBody removes request fields that the upstream
// OpenAI-compatible endpoint (e.g. Gemini at
// generativelanguage.googleapis.com/v1beta/openai) rejects with an opaque 400
// "Bad Request". Keeping the body clean prevents the gateway from forwarding
// fields that would otherwise surface to the client as
// "upstream_error: Bad Request".
//
// The function only removes keys; it never injects new fields, because Gemini
// also rejects explicit null values (e.g. "frequency_penalty": null) with the
// same 400.
func sanitizeOpenAIBody(payload map[string]any) map[string]any {
	// Gemini's OpenAI-compatible API has no logprobs support.
	delete(payload, "logprobs")
	delete(payload, "top_logprobs")
	// Gemini rejects the frequency / presence penalty parameters entirely,
	// even with valid in-range values.
	delete(payload, "frequency_penalty")
	delete(payload, "presence_penalty")

	// Gemini only supports a single completion (n=1).
	if n, ok := payload["n"]; ok {
		if f, ok := toFloat(n); ok && f > 1 {
			payload["n"] = 1
		} else if i, ok := n.(int); ok && i > 1 {
			payload["n"] = 1
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
