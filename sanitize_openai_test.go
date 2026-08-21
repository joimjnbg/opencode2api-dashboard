package main

import (
	"testing"
)

// TestSanitizeOpenAIBodyStripsUnsupportedFields verifies that fields Gemini's
// OpenAI-compatible endpoint rejects are removed or normalized before the
// request is forwarded upstream (these otherwise surface as opaque
// "upstream_error: Bad Request" 400s).
func TestSanitizeOpenAIBodyStripsUnsupportedFields(t *testing.T) {
	in := map[string]any{
		"model":             "gemini-3.7-flash",
		"messages":          []any{map[string]any{"role": "user", "content": "hi"}},
		"logprobs":          true,
		"top_logprobs":      5,
		"n":                 2,
		"frequency_penalty": 3.0,
		"presence_penalty":  -5.0,
		"temperature":       5.0,
	}

	out := sanitizeOpenAIBody(in)

	if _, ok := out["logprobs"]; ok {
		t.Error("logprobs must be stripped (Gemini rejects it)")
	}
	if _, ok := out["top_logprobs"]; ok {
		t.Error("top_logprobs must be stripped (Gemini rejects it)")
	}
	if _, ok := out["frequency_penalty"]; ok {
		t.Error("frequency_penalty must be stripped (Gemini rejects the param)")
	}
	if _, ok := out["presence_penalty"]; ok {
		t.Error("presence_penalty must be stripped (Gemini rejects the param)")
	}
	if n, ok := out["n"].(int); !ok || n != 1 {
		t.Errorf("n>1 must be forced to 1, got %v", out["n"])
	}
	if _, ok := out["messages"]; !ok {
		t.Error("legitimate fields like messages must be preserved")
	}
	if tp, ok := toFloat(out["temperature"]); !ok || tp != 5.0 {
		t.Errorf("temperature must be preserved (Gemini accepts it), got %v", out["temperature"])
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
}
