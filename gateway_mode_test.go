package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSupportedModelModeAware(t *testing.T) {
	cases := []struct {
		model string
		mode  UpstreamMode
		want  bool
	}{
		{"gemini-2.5-flash", ModeOpenCode, false},
		{"gemini-2.5-flash", ModeOpenAI, true},
		{"gpt-4o", ModeOpenCode, true},
		{"gpt-4o", ModeOpenAI, true},
		{"deepseek-v4-flash-free", ModeOpenCode, true},
	}
	for _, c := range cases {
		if got := supportedModel(c.model, c.mode); got != c.want {
			t.Errorf("supportedModel(%q, %q) = %v, want %v", c.model, c.mode, got, c.want)
		}
	}
}

func TestProtocolPathModeAware(t *testing.T) {
	cases := []struct {
		protocol Protocol
		mode     UpstreamMode
		want     string
	}{
		{ProtocolChat, ModeOpenCode, "/v1/chat/completions"},
		{ProtocolChat, ModeOpenAI, "/chat/completions"},
		{ProtocolResponses, ModeOpenCode, "/v1/responses"},
		{ProtocolResponses, ModeOpenAI, "/responses"},
		{ProtocolAnthropic, ModeOpenCode, "/v1/messages"},
		{ProtocolAnthropic, ModeOpenAI, "/messages"},
	}
	for _, c := range cases {
		if got := protocolPath(c.protocol, c.mode); got != c.want {
			t.Errorf("protocolPath(%q, %q) = %q, want %q", c.protocol, c.mode, got, c.want)
		}
	}
}

func TestIsQuotaErrorModeAware(t *testing.T) {
	cfg := FailoverConfig{Enabled: true, TreatGeneric429AsQuota: false}

	// OpenAI mode: a bare 429 is a per-minute rate limit, not quota -> throttle only.
	if isQuotaError(http.StatusTooManyRequests, []byte("rate limit exceeded"), cfg, ModeOpenAI) {
		t.Error("openai 429 with rate-limit body should not be a quota error")
	}
	// OpenAI mode: daily quota exhaustion body still matches.
	if !isQuotaError(http.StatusTooManyRequests, []byte("your quota has been exceeded"), cfg, ModeOpenAI) {
		t.Error("openai quota-exceeded body should be a quota error")
	}
	// OpenCode mode: a literal 429 body is a quota signal even without TreatGeneric429AsQuota.
	if !isQuotaError(http.StatusTooManyRequests, []byte("HTTP 429"), cfg, ModeOpenCode) {
		t.Error("opencode 429 body should be a quota error")
	}
	// TreatGeneric429AsQuota flips the bare 429 in either mode.
	generic := FailoverConfig{Enabled: true, TreatGeneric429AsQuota: true}
	if !isQuotaError(http.StatusTooManyRequests, []byte(""), generic, ModeOpenAI) {
		t.Error("TreatGeneric429AsQuota should make bare 429 a quota error")
	}
}

func TestLoadConfigOpenAIMode(t *testing.T) {
	dir := t.TempDir()
	valid := `{"listen":"127.0.0.1:8080","server_keys":["k"],"zen_keys":["gk"],"upstream_mode":"openai","upstream":{"zen":"https://generativelanguage.googleapis.com/v1beta/openai"},"models":{"static":["gemini-2.5-flash"]}}`
	bad := `{"listen":"127.0.0.1:8080","server_keys":["k"],"zen_keys":["gk"],"upstream_mode":"openai","upstream":{"zen":""}}`
	if _, err := LoadConfig(writeTemp(t, dir, "valid.json", valid)); err != nil {
		t.Errorf("valid openai config rejected: %v", err)
	}
	if _, err := LoadConfig(writeTemp(t, dir, "bad.json", bad)); err == nil {
		t.Error("openai config without upstream.zen should be rejected")
	}
}

// TestLoadConfigOpenAIStaticGo verifies the second-upstream static list
// (models.static_go) parses and reaches the config.
func TestLoadConfigOpenAIStaticGo(t *testing.T) {
	dir := t.TempDir()
	raw := `{"listen":"127.0.0.1:8080","server_keys":["k"],"zen_keys":["gk"],"go_keys":["lk"],"upstream_mode":"openai","upstream":{"zen":"https://gemini.example","go":"https://relay.example"},"models":{"static":["gemini-2.5-flash"],"static_go":["claude-opus-5","deepseek-v4-flash"]}}`
	cfg, err := LoadConfig(writeTemp(t, dir, "cfg.json", raw))
	if err != nil {
		t.Fatalf("valid openai static_go config rejected: %v", err)
	}
	if len(cfg.Models.Static) != 1 || cfg.Models.Static[0] != "gemini-2.5-flash" {
		t.Errorf("static list not parsed: %v", cfg.Models.Static)
	}
	if len(cfg.Models.StaticGo) != 2 || cfg.Models.StaticGo[0] != "claude-opus-5" {
		t.Errorf("static_go list not parsed: %v", cfg.Models.StaticGo)
	}
}

// TestRouteSecondUpstreamTiers verifies models from each static list route to
// their own tier: gemini-* -> zen (primary OpenAI-compatible upstream),
// relay models -> go (second upstream with its own key pool).
func TestRouteSecondUpstreamTiers(t *testing.T) {
	catalog := newModelCatalog(TierZen, map[string]string{}, ModeOpenAI)
	catalog.Replace(
		[]string{"gemini-3.7-flash", "gemini-2.5-flash"},
		[]string{"big-pickle", "claude-opus-5", "deepseek-v4-flash"},
	)

	route, err := catalog.Route("gemini-3.7-flash", true, true)
	if err != nil || route.Tier != TierZen {
		t.Errorf("gemini model must route to zen tier, got tier=%v err=%v", route.Tier, err)
	}
	route, err = catalog.Route("deepseek-v4-flash", true, true)
	if err != nil || route.Tier != TierGo {
		t.Errorf("relay model must route to go tier, got tier=%v err=%v", route.Tier, err)
	}
	route, err = catalog.Route("claude-opus-5", false, true)
	if err != nil || route.Tier != TierGo {
		t.Errorf("relay model routes even without zen keys, got tier=%v err=%v", route.Tier, err)
	}

	// prefer=go must not change the outcome when model sets do not overlap.
	catalogPreferGo := newModelCatalog(TierGo, map[string]string{}, ModeOpenAI)
	catalogPreferGo.Replace([]string{"gemini-3.7-flash"}, []string{"deepseek-v4-flash"})
	route, err = catalogPreferGo.Route("gemini-3.7-flash", true, true)
	if err != nil || route.Tier != TierZen {
		t.Errorf("prefer=go must not steal zen-only models, got tier=%v err=%v", route.Tier, err)
	}

	// Unknown models stay rejected.
	if _, err := catalog.Route("nonexistent-model", true, true); err == nil {
		t.Error("unknown model must be rejected when both catalogs are populated")
	}
}
