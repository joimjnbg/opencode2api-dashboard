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

// TestSanitizeOpenAIBodyStripsModernUnsupportedFields covers params that
// newer OpenAI-shaped clients (e.g. the opencode ai-sdk) send but Gemini's
// OpenAI-compatible endpoint rejects with an opaque 400 "Bad Request".
func TestSanitizeOpenAIBodyStripsModernUnsupportedFields(t *testing.T) {
	in := map[string]any{
		"model":            "gemini-3.7-flash",
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens":       10,
		"function_call":    "none",
		"functions":        []any{},
		"metadata":         map[string]any{"a": "b"},
		"reasoning_effort": "low",
		"service_tier":     "default",
	}
	out := sanitizeOpenAIBody(in)
	for _, k := range []string{"function_call", "functions", "metadata", "reasoning_effort", "service_tier"} {
		if _, ok := out[k]; ok {
			t.Errorf("%s must be stripped (Gemini rejects it)", k)
		}
	}
	if _, ok := out["max_tokens"]; !ok {
		t.Error("legitimate max_tokens must be preserved")
	}
}

// TestSanitizeOpenAIBodyStripsGeminiNativeParams covers the Gemini-native
// GenerateContent params that clients targeting Gemini sometimes send but the
// OpenAI-compatible endpoint rejects with 400 "Bad Request".
func TestSanitizeOpenAIBodyStripsGeminiNativeParams(t *testing.T) {
	in := map[string]any{
		"model":           "gemini-3.7-flash",
		"messages":        []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens":      10,
		"top_k":           20,
		"stop_sequences":  []any{"STOP"},
		"candidate_count": 2,
		"safety_settings": []any{},
	}
	out := sanitizeOpenAIBody(in)
	for _, k := range []string{"top_k", "stop_sequences", "candidate_count", "safety_settings"} {
		if _, ok := out[k]; ok {
			t.Errorf("%s must be stripped (Gemini OpenAI endpoint rejects it)", k)
		}
	}
	if _, ok := out["max_tokens"]; !ok {
		t.Error("legitimate max_tokens must be preserved")
	}
}

// TestSanitizeOpenAIBodyRenamesMaxOutputTokens verifies Gemini's own output-cap
// param is also rewritten to max_tokens.
func TestSanitizeOpenAIBodyRenamesMaxOutputTokens(t *testing.T) {
	in := map[string]any{
		"model":             "gemini-3.7-flash",
		"messages":          []any{map[string]any{"role": "user", "content": "hi"}},
		"max_output_tokens": 8,
	}
	out := sanitizeOpenAIBody(in)
	if _, ok := out["max_output_tokens"]; ok {
		t.Error("max_output_tokens should be removed")
	}
	if f, ok := toFloat(out["max_tokens"]); !ok || f != 8 {
		t.Errorf("expected max_tokens=8, got %v", out["max_tokens"])
	}
}

// TestSanitizeOpenAIBodyRenamesMaxCompletionTokens verifies the modern OpenAI
// output-cap param is rewritten to Gemini's "max_tokens".
func TestSanitizeOpenAIBodyRenamesMaxCompletionTokens(t *testing.T) {
	in := map[string]any{
		"model":                 "gemini-3.7-flash",
		"messages":              []any{map[string]any{"role": "user", "content": "hi"}},
		"max_completion_tokens": 16,
	}
	out := sanitizeOpenAIBody(in)
	if _, ok := out["max_completion_tokens"]; ok {
		t.Error("max_completion_tokens should be removed")
	}
	if f, ok := toFloat(out["max_tokens"]); !ok || f != 16 {
		t.Errorf("expected max_tokens=16, got %v", out["max_tokens"])
	}

	// When max_tokens is already present, only drop the alias.
	in2 := map[string]any{"max_tokens": 5, "max_completion_tokens": 16}
	out2 := sanitizeOpenAIBody(in2)
	if f, ok := toFloat(out2["max_tokens"]); !ok || f != 5 {
		t.Errorf("max_tokens should be untouched, got %v", out2["max_tokens"])
	}
	if _, ok := out2["max_completion_tokens"]; ok {
		t.Error("max_completion_tokens should be removed even when max_tokens present")
	}
}

// TestSanitizeOpenAIBodyClampsTemperature verifies Gemini's [0,2] temperature
// range is enforced by clamping rather than dropping the request.
func TestSanitizeOpenAIBodyClampsTemperature(t *testing.T) {
	in := map[string]any{
		"model":       "gemini-3.7-flash",
		"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
		"temperature": -0.5,
	}
	out := sanitizeOpenAIBody(in)
	if f, ok := toFloat(out["temperature"]); !ok || f != 0 {
		t.Errorf("temperature -0.5 should clamp to 0, got %v", out["temperature"])
	}
}

// TestSanitizeOpenAIBodyFoldsSystemField verifies a top-level "system" field
// is folded into the messages array as a system role instead of being rejected.
func TestSanitizeOpenAIBodyFoldsSystemField(t *testing.T) {
	in := map[string]any{
		"model":    "gemini-3.7-flash",
		"system":   "you are helpful",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out := sanitizeOpenAIBody(in)
	if _, ok := out["system"]; ok {
		t.Error("top-level system should be removed")
	}
	msgs, ok := out["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %v", out["messages"])
	}
	first, ok := msgs[0].(map[string]any)
	if !ok || first["role"] != "system" || first["content"] != "you are helpful" {
		t.Errorf("first message should be the folded system prompt, got %v", msgs[0])
	}
}
