package main

import (
	"testing"
)

// TestSanitizeOpenAIBodyStripsUnsupportedFields verifies that every field
// Gemini's OpenAI-compatible endpoint rejects is removed (or normalized) before
// the request is forwarded, so the client never sees an opaque
// "upstream_error: Bad Request" 400.
func TestSanitizeOpenAIBodyStripsUnsupportedFields(t *testing.T) {
	in := map[string]any{
		"model":              "gemini-3.7-flash",
		"messages":           []any{map[string]any{"role": "user", "content": "hi"}},
		"logprobs":           true,
		"top_logprobs":       5,
		"logit_bias":         map[string]any{"123": 5},
		"seed":               42,
		"echo":               true,
		"best_of":            2,
		"frequency_penalty":  3.0,
		"presence_penalty":   -5.0,
		"repetition_penalty": 1.1,
		"n":                  2,
		"top_p":              2.5,
	}

	out := sanitizeOpenAIBody(in)

	for _, key := range geminiUnsupportedParams {
		if _, ok := out[key]; ok {
			t.Errorf("%s must be stripped (Gemini rejects it)", key)
		}
	}
	if n, ok := out["n"].(int); !ok || n != 1 {
		t.Errorf("n>1 must be forced to 1, got %v", out["n"])
	}
	if tp, ok := out["top_p"].(float64); !ok || tp != 1.0 {
		t.Errorf("top_p 2.5 must be clamped to 1.0, got %v", out["top_p"])
	}
	if _, ok := out["messages"]; !ok {
		t.Error("legitimate fields like messages must be preserved")
	}
}

// TestSanitizeOpenAIBodyPreservesValidRequest ensures a well-formed request is
// passed through untouched (no over-stripping).
func TestSanitizeOpenAIBodyPreservesValidRequest(t *testing.T) {
	in := map[string]any{
		"model":       "gemini-3.7-flash",
		"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
		"temperature": 0.7,
		"top_p":       0.9,
		"max_tokens":  1024,
		"stream":      true,
	}
	out := sanitizeOpenAIBody(in)

	if f, ok := toFloat(out["temperature"]); !ok || f != 0.7 {
		t.Errorf("temperature 0.7 must be preserved, got %v", out["temperature"])
	}
	if f, ok := toFloat(out["top_p"]); !ok || f != 0.9 {
		t.Errorf("top_p must be preserved, got %v", out["top_p"])
	}
	if f, ok := toFloat(out["max_tokens"]); !ok || f != 1024 {
		t.Errorf("max_tokens must be preserved, got %v", out["max_tokens"])
	}
	if _, ok := out["stream"]; !ok {
		t.Error("stream must be preserved")
	}
}
