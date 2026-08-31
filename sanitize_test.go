package main

import (
	"testing"
)

func TestSanitizeModelStripsFreeSuffix(t *testing.T) {
	cfg := SanitizeConfig{Enabled: true, StripFreeSuffix: true}
	cases := map[string]string{
		"deepseek-v4-flash-free":     "deepseek-v4-flash",
		"hy3-free":                   "hy3",
		"deepseek-v4-flash-zen-free": "deepseek-v4-flash",
		"claude-sonnet-4-5-free":     "claude-sonnet-4-5",
		"plain-model":                "plain-model",
		"Free":                       "",
	}
	for input, want := range cases {
		if got := sanitizeModel(input, cfg); got != want {
			t.Errorf("sanitizeModel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSanitizeModelAliasWins(t *testing.T) {
	cfg := SanitizeConfig{
		Enabled:         true,
		StripFreeSuffix: true,
		ModelAliases:    map[string]string{"deepseek-v4-flash-free": "deepseek-v4-flash"},
	}
	if got := sanitizeModel("deepseek-v4-flash-free", cfg); got != "deepseek-v4-flash" {
		t.Errorf("alias not applied: got %q", got)
	}
}

func TestSanitizeModelDisabled(t *testing.T) {
	cfg := SanitizeConfig{Enabled: false}
	if got := sanitizeModel("hy3-free", cfg); got != "hy3-free" {
		t.Errorf("sanitize should be off: got %q", got)
	}
}
