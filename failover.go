package main

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
)

// quotaErrorPatternsOpenCode match opencode.ai bodies that signal free-quota
// exhaustion. The upstream may return them under HTTP 200, 400, 402, 403 or 429
// depending on the protocol, so matching must inspect the body, not just the
// status. The bare "429" pattern lets opencode's literal 429 bodies trigger
// quota cooldown even when TreatGeneric429AsQuota is false.
var quotaErrorPatternsOpenCode = []*regexp.Regexp{
	regexp.MustCompile(`(?i)free usage exceeded`),
	regexp.MustCompile(`(?i)subscribe to go`),
	regexp.MustCompile(`(?i)rate_limit_exceeded`),
	regexp.MustCompile(`(?i)quota.*(exceeded|exhausted|reached)`),
	regexp.MustCompile(`(?i)(no|insufficient|out of).*(quota|balance|credit)`),
	regexp.MustCompile(`(?i)429`),
}

// quotaErrorPatternsOpenAI match OpenAI-compatible upstreams (Gemini). A bare
// "429" is deliberately absent: a generic 429 there is a per-minute rate limit
// and should throttle only that key (see doUpstream), not cool the whole
// account. Daily-quota exhaustion still matches via the quota phrases.
var quotaErrorPatternsOpenAI = []*regexp.Regexp{
	regexp.MustCompile(`(?i)rate_limit_exceeded`),
	regexp.MustCompile(`(?i)quota.*(exceeded|exhausted|reached)`),
	regexp.MustCompile(`(?i)(no|insufficient|out of).*(quota|balance|credit)`),
}

// retryableStatus marks upstream status codes that are allowed to silently
// switch to the next account key. Everything else (malformed request, unknown
// model) is returned to the client unchanged.
func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests,
		http.StatusPaymentRequired, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return status >= 500
}

// isQuotaError reports whether a captured upstream body is a free-quota
// exhaustion signal that should trigger silent failover to another key.
func isQuotaError(status int, body []byte, cfg FailoverConfig, mode UpstreamMode) bool {
	if !cfg.Enabled {
		return false
	}
	if status == http.StatusPaymentRequired {
		return true
	}
	if status == http.StatusTooManyRequests && cfg.TreatGeneric429AsQuota {
		return true
	}
	patterns := quotaErrorPatternsOpenCode
	if mode.isOpenAI() {
		patterns = quotaErrorPatternsOpenAI
	}
	lower := bytes.ToLower(body)
	for _, pattern := range patterns {
		if pattern.Match(lower) {
			return true
		}
	}
	return false
}

// capturedResponse is a failed upstream response whose body was drained so the
// gateway can inspect it for quota signals without blocking the connection.
type capturedResponse struct {
	status int
	header http.Header
	body   []byte
}

// captureBody drains an upstream response body (bounded) and closes it, so the
// caller can inspect and reuse the payload.
func captureBody(resp *http.Response, limitBytes int64) capturedResponse {
	out := capturedResponse{status: resp.StatusCode, header: resp.Header.Clone()}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, limitBytes))
	_ = resp.Body.Close()
	out.body = body
	return out
}

// fallbackModel returns the client's original model name when the sanitized
// name fails to route.
func (g *Gateway) routeWithSanitize(model string, hasZen, hasGo bool, cfg SanitizeConfig) (modelRoute, string, error) {
	cleaned := sanitizeModel(model, cfg)
	if cleaned != "" && cleaned != model {
		if route, err := g.catalog.Route(cleaned, hasZen, hasGo); err == nil {
			return route, cleaned, nil
		}
	}
	route, err := g.catalog.Route(model, hasZen, hasGo)
	return route, model, err
}
