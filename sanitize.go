package main

import (
	"regexp"
	"strings"
)

// freeSuffixPattern matches "-free", "-zen-free", "-zenfree", etc.
//
// NOTE: stripping the suffix is NOT enabled by default. On opencode.ai the
// "-free" suffix is the free model's identity: "deepseek-v4-flash-free" is the
// free model while "deepseek-v4-flash" requires a payment method. Stripping
// the suffix therefore turns free requests into paid ones and triggers
// 401 "No payment method" from the upstream. Use model_aliases for remapping.
var freeSuffixPattern = regexp.MustCompile(`(?i)(-zen)?-?free$`)

// sanitizeModel maps a client-supplied model identifier onto the canonical
// identifier for the account pool. It applies explicit aliases first, then
// strips free-tier suffixes. The callers validate that the resulting model is
// still routable, and fall back to the original name when it is not.
func sanitizeModel(model string, cfg SanitizeConfig) string {
	if model == "" || !cfg.Enabled {
		return model
	}
	if alias, ok := cfg.ModelAliases[model]; ok && alias != "" {
		return alias
	}
	if cfg.StripFreeSuffix {
		return freeSuffixPattern.ReplaceAllString(model, "")
	}
	return model
}

// sanitizeRequestHeader cleans obvious free-tier markers from a request body
// in place. It handles the top-level "model" field for chat/responses and the
// Anthropic message shape.
func sanitizeRequestModel(payload map[string]any, cfg SanitizeConfig) string {
	model := stringAt(payload, "model")
	if model == "" {
		return ""
	}
	cleaned := sanitizeModel(model, cfg)
	if cleaned != "" && cleaned != model {
		payload["model"] = cleaned
	}
	return cleaned
}

var _ = strings.TrimSpace
