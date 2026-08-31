package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxRequestBody = 32 << 20

const (
	proxyHealthCheckURL      = "https://cloudflare.com/cdn-cgi/trace"
	proxyHealthCheckInterval = 15 * time.Minute
	proxyHealthCheckTimeout  = 10 * time.Second
)

type Gateway struct {
	cfg         Config
	logger      *slog.Logger
	transports  *transportPool
	zenNodes    *nodePool
	goNodes     *nodePool
	catalog     *modelCatalog
	stats       *usageStats
	fingerprint *fingerprintStore
}

type healthResponse struct {
	Status  string        `json:"status"`
	Ready   bool          `json:"ready"`
	Version string        `json:"version"`
	Models  healthModels  `json:"models"`
	Keys    healthKeys    `json:"keys"`
	Proxies healthProxies `json:"proxies"`
	Issues  []string      `json:"issues,omitempty"`
}

type healthModels struct {
	Status            string     `json:"status"`
	Total             int        `json:"total"`
	Exposed           int        `json:"exposed"`
	Zen               int        `json:"zen"`
	Go                int        `json:"go"`
	LastRefresh       *time.Time `json:"last_refresh,omitempty"`
	StaleAfterSeconds int        `json:"stale_after_seconds"`
}

type healthKeys struct {
	Zen        int         `json:"zen"`
	Go         int         `json:"go"`
	Total      int         `json:"total"`
	ZenStatus  []keyStatus `json:"zen_status,omitempty"`
	GoStatus   []keyStatus `json:"go_status,omitempty"`
	Throttled  string      `json:"throttled,omitempty"`           // "zen" | "go" | "both" | ""
	ThrottleIn int         `json:"throttle_in_seconds,omitempty"` // seconds until the account throttle window ends
}

type healthProxies struct {
	Total     int `json:"total"`
	Healthy   int `json:"healthy"`
	Unhealthy int `json:"unhealthy"`
}

func NewGateway(cfg Config, logger *slog.Logger) (*Gateway, error) {
	transports, err := newTransportPool(cfg.Proxies, cfg.Performance, time.Duration(cfg.Retry.TimeoutSeconds)*time.Second)
	if err != nil {
		return nil, err
	}
	cooldown := time.Duration(cfg.Performance.FailureCooldownSeconds) * time.Second
	zenNodes, err := newNodePool(cfg.ZenKeys, transports, cooldown)
	if err != nil {
		return nil, fmt.Errorf("zen node pool: %w", err)
	}
	goNodes, err := newNodePool(cfg.GoKeys, transports, cooldown)
	if err != nil {
		return nil, fmt.Errorf("go node pool: %w", err)
	}
	// The go tier is the last-resort fallback. Cap its per-key cooldown at the
	// base so transient relay errors cannot grow a key's backoff past the
	// fallback window and knock the safety net offline.
	goNodes.maxCooldown = cooldown
	gateway := &Gateway{
		cfg:        cfg,
		logger:     logger,
		transports: transports,
		zenNodes:   zenNodes,
		goNodes:    goNodes,
		catalog:    newModelCatalog(cfg.Prefer, cfg.Models.Protocols, cfg.UpstreamMode),
		stats:      newUsageStats(),
	}
	zenNodes.SetMultiAccount(cfg.Failover.MultiAccount)
	goNodes.SetMultiAccount(cfg.Failover.MultiAccount)
	if cfg.Fingerprint.Enabled {
		gateway.fingerprint = newFingerprintStore(cfg.Fingerprint.PersistFile)
		zenNodes.SetFingerprints(gateway.fingerprint)
		goNodes.SetFingerprints(gateway.fingerprint)
	}
	if cfg.Stats.AuditFile != "" {
		gateway.stats.SetAudit(newAuditWriter(cfg.Stats.AuditFile))
	}
	// Periodically prune old hourly stats to prevent unbounded memory growth
	// in long-running gateways.
	gateway.stats.StartPrune(context.Background(), 48*time.Hour, time.Hour)
	return gateway, nil
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", g.authenticate(g.handleModels))
	mux.HandleFunc("POST /v1/chat/completions", g.authenticate(g.handleInference(ProtocolChat)))
	mux.HandleFunc("POST /v1/responses", g.authenticate(g.handleInference(ProtocolResponses)))
	mux.HandleFunc("POST /v1/messages", g.authenticate(g.handleInference(ProtocolAnthropic)))
	mux.HandleFunc("GET /v1/stats", g.authenticate(g.handleStats))
	mux.HandleFunc("GET /metrics", g.handleMetrics)
	mux.HandleFunc("GET /healthz", g.handleHealth)
	return recoveryMiddleware(g.logger, mux)
}

func (g *Gateway) handleStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, g.stats.Snapshot())
}

func (g *Gateway) handleHealth(w http.ResponseWriter, _ *http.Request) {
	models := g.catalog.Snapshot()
	proxyTotal, proxyHealthy := g.transports.healthCounts()
	zenKeys, goKeys := g.zenNodes.Len(), g.goNodes.Len()
	staleAfter := max(2*time.Duration(g.cfg.Models.RefreshSeconds)*time.Second, time.Minute)

	modelStatus := "ready"
	var lastRefresh *time.Time
	issues := make([]string, 0, 3)
	if models.UpdatedAt.IsZero() {
		modelStatus = "pending"
		issues = append(issues, "model_catalog_pending")
	} else {
		updatedAt := models.UpdatedAt.UTC()
		lastRefresh = &updatedAt
		if models.Exposed == 0 {
			modelStatus = "empty"
			issues = append(issues, "model_catalog_empty")
		} else if time.Since(models.UpdatedAt) > staleAfter {
			modelStatus = "stale"
			issues = append(issues, "model_catalog_stale")
		}
	}
	if zenKeys+goKeys == 0 {
		issues = append(issues, "no_upstream_keys")
	}
	if proxyHealthy == 0 {
		issues = append(issues, "no_healthy_proxies")
	}

	status := "ok"
	httpStatus := http.StatusOK
	if len(issues) > 0 {
		status = "degraded"
		httpStatus = http.StatusServiceUnavailable
		if modelStatus == "pending" {
			status = "starting"
		}
	}
	throttled := ""
	var throttleIn int
	zenThrottle := g.zenNodes.ThrottleDeadline()
	goThrottle := g.goNodes.ThrottleDeadline()
	now := time.Now()
	switch {
	case !zenThrottle.IsZero() && !goThrottle.IsZero():
		throttled = "both"
	case !zenThrottle.IsZero():
		throttled = "zen"
	case !goThrottle.IsZero():
		throttled = "go"
	}
	earliest := zenThrottle
	if !goThrottle.IsZero() && (earliest.IsZero() || goThrottle.Before(earliest)) {
		earliest = goThrottle
	}
	if !earliest.IsZero() {
		throttleIn = max(int(earliest.Sub(now).Seconds()), 0)
	}

	writeJSON(w, httpStatus, healthResponse{
		Status:  status,
		Ready:   len(issues) == 0,
		Version: version,
		Models: healthModels{
			Status:            modelStatus,
			Total:             models.Total,
			Exposed:           models.Exposed,
			Zen:               models.Zen,
			Go:                models.Go,
			LastRefresh:       lastRefresh,
			StaleAfterSeconds: int(staleAfter / time.Second),
		},
		Keys: healthKeys{
			Zen:        zenKeys,
			Go:         goKeys,
			Total:      zenKeys + goKeys,
			ZenStatus:  g.zenNodes.StatusSnapshot(),
			GoStatus:   g.goNodes.StatusSnapshot(),
			Throttled:  throttled,
			ThrottleIn: throttleIn,
		},
		Proxies: healthProxies{
			Total:     proxyTotal,
			Healthy:   proxyHealthy,
			Unhealthy: proxyTotal - proxyHealthy,
		},
		Issues: issues,
	})
}

func (g *Gateway) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		candidates := []string{strings.TrimSpace(r.Header.Get("x-api-key"))}
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			candidates = append(candidates, strings.TrimSpace(auth[7:]))
		}
		valid := false
	KeyLoop:
		for _, key := range g.cfg.ServerKeys {
			for _, candidate := range candidates {
				if len(candidate) == len(key) && subtle.ConstantTimeCompare([]byte(candidate), []byte(key)) == 1 {
					valid = true
					break KeyLoop
				}
			}
		}
		if !valid {
			protocol := ProtocolChat
			if r.URL.Path == "/v1/messages" {
				protocol = ProtocolAnthropic
			}
			writeAPIError(w, protocol, http.StatusUnauthorized, "invalid local API key", "authentication_error", "")
			return
		}
		next(w, r)
	}
}

func (g *Gateway) handleModels(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().Unix()
	models := g.catalog.List()
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		if !supportedModel(model, g.cfg.UpstreamMode) {
			continue
		}
		ownedBy := "opencode"
		if g.cfg.UpstreamMode.isOpenAI() {
			ownedBy = "google"
		}
		data = append(data, map[string]any{"id": model, "object": "model", "created": now, "owned_by": ownedBy})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (g *Gateway) handleInference(external Protocol) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
		if err != nil {
			writeAPIError(w, external, http.StatusBadRequest, "request body is too large or unreadable", "invalid_request_error", "")
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			writeAPIError(w, external, http.StatusBadRequest, "request body must be a JSON object", "invalid_request_error", "")
			return
		}
		model := stringAt(payload, "model")
		if model == "" {
			writeAPIError(w, external, http.StatusBadRequest, "model is required", "invalid_request_error", "model")
			return
		}
		if !supportedModel(model, g.cfg.UpstreamMode) {
			writeAPIError(w, external, http.StatusBadRequest, "the model uses an upstream protocol that opencode2api does not expose", "invalid_request_error", "model")
			return
		}
		route, routedModel, err := g.routeWithSanitize(model, len(g.cfg.ZenKeys) > 0, len(g.cfg.GoKeys) > 0, g.cfg.Sanitize)
		if err != nil {
			writeAPIError(w, external, http.StatusBadRequest, err.Error(), "invalid_request_error", "model")
			return
		}
		if routedModel != "" && routedModel != model {
			payload["model"] = routedModel
			model = routedModel
		}
		if g.cfg.UpstreamMode.isOpenAI() {
			sanitizeOpenAIBody(payload)
		}
		ids := deriveRequestIDs(r, payload)
		stream := boolAt(payload, "stream")
		g.logger.Debug("inference request", "request_id", ids.Request, "client_model", model, "tier", route.Tier, "protocol", route.Protocol, "external", external, "stream", stream)
		requestCtx, cancel := context.WithTimeout(r.Context(), time.Duration(g.cfg.Retry.TimeoutSeconds)*time.Second)
		defer cancel()
		resp, err := g.doUpstreamWithFallback(requestCtx, route, external, payload, ids)
		if err != nil {
			var te *throttleError
			switch {
			case errors.As(err, &te):
				g.logger.Warn("account rate limited, returning 503", "request_id", ids.Request, "tier", route.Tier, "retry_after", te.retryAfter)
				g.stats.Record(model, bridgeUsage{}, 0, false)
				w.Header().Set("Retry-After", fmt.Sprintf("%d", int(te.retryAfter.Seconds())+1))
				writeAPIError(w, external, http.StatusServiceUnavailable, "upstream account is rate limited; retry later", "rate_limit_exceeded", ids.Request)
				return
			case errors.Is(err, errQuotaExhausted):
				g.logger.Warn("all upstream keys in quota cooldown", "request_id", ids.Request, "tier", route.Tier, "model", model)
				g.stats.Record(model, bridgeUsage{}, 0, false)
				if retry := g.quotaRetryAfter(route.Tier); retry > 0 {
					w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
				}
				writeAPIError(w, external, http.StatusServiceUnavailable, "all upstream keys are cooling down due to quota limits; retry later", "rate_limit_exceeded", ids.Request)
				return
			case errors.Is(err, errAllCooling):
				g.logger.Warn("all upstream keys cooling down or unavailable", "request_id", ids.Request, "tier", route.Tier, "model", model)
				g.stats.Record(model, bridgeUsage{}, 0, false)
				w.Header().Set("Retry-After", "5")
				writeAPIError(w, external, http.StatusServiceUnavailable, "all upstream keys are temporarily unavailable; retry later", "rate_limit_exceeded", ids.Request)
				return
			}
			g.logger.Warn("upstream request failed", "request_id", ids.Request, "tier", route.Tier, "error", err)
			g.stats.Record(model, bridgeUsage{}, 0, false)
			writeAPIError(w, external, http.StatusBadGateway, "all upstream attempts failed", "upstream_error", ids.Request)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("x-request-id", ids.Request)
		if resp.StatusCode/100 != 2 {
			copyErrorResponse(w, external, resp, ids.Request)
			return
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(resp.StatusCode)
			usageReader := newUsageExtractReader(resp.Body, route.Protocol, model, g.stats)
			if external == route.Protocol {
				_, err = io.Copy(w, usageReader)
			} else {
				err = transcodeStream(w, usageReader, route.Protocol, external, model)
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				g.logger.Debug("stream ended with error", "request_id", ids.Request, "error", err)
			}
			return
		}
		responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			writeAPIError(w, external, http.StatusBadGateway, "failed to read upstream response", "upstream_error", ids.Request)
			return
		}
		if resp.StatusCode >= 400 {
			msg := string(responseBody)
			if len(msg) > 600 {
				msg = msg[:600]
			}
			if dump, derr := json.Marshal(payload); derr == nil {
				d := string(dump)
				if len(d) > 1200 {
					d = d[:1200]
				}
				g.logger.Warn("upstream returned error status", "request_id", ids.Request, "tier", route.Tier, "model", payload["model"], "status", resp.StatusCode, "body", msg, "sent", d)
			} else {
				g.logger.Warn("upstream returned error status", "request_id", ids.Request, "tier", route.Tier, "model", payload["model"], "status", resp.StatusCode, "body", msg)
			}
		}
		g.stats.Record(model, usageFromResponse(route.Protocol, responseBody), costFromResponse(responseBody), true)
		if external != route.Protocol {
			responseBody, err = convertResponse(route.Protocol, external, responseBody)
			if err != nil {
				g.logger.Warn("response conversion failed", "request_id", ids.Request, "error", err)
				writeAPIError(w, external, http.StatusBadGateway, "unsupported upstream response", "upstream_error", ids.Request)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(responseBody)
	}
}

// doUpstreamWithFallback prepares and sends the request to the model's own
// tier, then — when the primary (zen) tier is exhausted (all keys
// quota-parked/cooled, the pool throttled, or nothing cooling-free left) and a
// fallback model is configured — retries the same request once against the
// second upstream (go tier) under the configured substitute model name. The
// client keeps speaking its original model; usage stats stay on that name.
func (g *Gateway) doUpstreamWithFallback(ctx context.Context, route modelRoute, external Protocol, payload map[string]any, ids requestIDs) (*http.Response, error) {
	baseURL := g.cfg.Upstream.Zen
	if route.Tier == TierGo {
		baseURL = g.cfg.Upstream.Go
	}
	prepared, err := prepareUpstreamRequest(external, route.Protocol, payload, baseURL)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(prepared)
	if err != nil {
		return nil, err
	}

	// When a cross-tier fallback exists, bound how long we wait on the primary
	// tier (backpressure waits + cooldowns) before giving up and failing over,
	// so the client isn't held for the full request timeout when the primary is
	// down. The fallback request gets its own fresh, full-length context.
	fallbackEligible := route.Tier == TierZen && g.cfg.Failover.CrossTierFallbackModel != "" && len(g.cfg.GoKeys) > 0
	primaryCtx := ctx
	primaryCancel := func() {}
	if fallbackEligible {
		cap := 10 * time.Second
		if d := time.Duration(g.cfg.Retry.TimeoutSeconds) * time.Second / 2; d > 0 && d < cap {
			cap = d
		}
		primaryCtx, primaryCancel = context.WithTimeout(ctx, cap)
	}
	resp, err := g.doUpstream(primaryCtx, route, encoded, ids)
	primaryCancel()
	if err == nil || !fallbackEligible {
		return resp, err
	}
	var te *throttleError
	allowFallback := errors.As(err, &te) || errors.Is(err, errQuotaExhausted) || errors.Is(err, errAllCooling) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	if !allowFallback {
		return resp, err
	}

	fallbackModel := g.cfg.Failover.CrossTierFallbackModel
	g.logger.Warn("primary tier exhausted, failing over to second upstream", "request_id", ids.Request, "from_model", stringAt(payload, "model"), "to_model", fallbackModel, "reason", err.Error())
	altRoute := modelRoute{ID: fallbackModel, Tier: TierGo, Protocol: ProtocolChat}
	payload["model"] = fallbackModel
	altPrepared, perr := prepareUpstreamRequest(external, altRoute.Protocol, payload, g.cfg.Upstream.Go)
	if perr != nil {
		return resp, err
	}
	altEncoded, merr := json.Marshal(altPrepared)
	if merr != nil {
		return resp, err
	}
	// Use a fresh timeout derived from the original request context so that:
	// (1) the primary tier's backpressure wait cannot cancel the fallback via
	//     the old deadline, and (2) client disconnection still propagates.
	fbCtx, fbCancel := context.WithTimeout(ctx, time.Duration(g.cfg.Retry.TimeoutSeconds)*time.Second)
	defer fbCancel()
	altResp, aerr := g.doUpstream(fbCtx, altRoute, altEncoded, ids)
	if aerr != nil {
		g.logger.Warn("cross-tier fallback also failed", "request_id", ids.Request, "to_model", fallbackModel, "fallback_error", aerr.Error(), "primary_error", err.Error())
		// Both tiers are out: surface the original primary-tier error so the
		// client gets the more meaningful rate-limit signal.
		return resp, err
	}
	return altResp, nil
}

func (g *Gateway) doUpstream(ctx context.Context, route modelRoute, body []byte, ids requestIDs) (*http.Response, error) {
	nodes := g.zenNodes
	baseURL := g.cfg.Upstream.Zen
	if route.Tier == TierGo {
		nodes = g.goNodes
		baseURL = g.cfg.Upstream.Go
	}
	if nodes.Len() == 0 {
		return nil, fmt.Errorf("no %s nodes configured", route.Tier)
	}

	quotaCooldown := time.Duration(g.cfg.Failover.QuotaCooldownMinutes) * time.Minute
	if quotaCooldown <= 0 {
		quotaCooldown = 30 * time.Minute
	}

	// Backpressure: when the whole account is throttled or every key is
	// cooling down, hold the request (bounded) instead of failing it right
	// away. The client hangs for up to MaxWaitSeconds while the rate-limit
	// window elapses, then one probe request (half-open) decides whether the
	// pool recovered.
	maxWait := time.Duration(g.cfg.Failover.Throttle.MaxWaitSeconds) * time.Second
	if maxWait <= 0 {
		maxWait = 60 * time.Second
	}
	deadline := time.Now().Add(maxWait)
	sharedWindow := time.Duration(g.cfg.Failover.Throttle.InitialSeconds) * time.Second
	if sharedWindow <= 0 {
		sharedWindow = 60 * time.Second
	}

	var lastResponse *http.Response
	var lastErr error
	anyQuota := false
	attempt := 0
	maxAttempts := g.cfg.Retry.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

requestLoop:
	for {
		// Wait out an account-level throttle window before trying any key.
		if until := nodes.ThrottleDeadline(); !until.IsZero() {
			if until.After(deadline) {
				return nil, &throttleError{retryAfter: time.Until(until)}
			}
			g.logger.Debug("account rate limit active, holding request", "request_id", ids.Request, "tier", route.Tier, "wait_seconds", int(time.Until(until).Seconds()))
			if !sleepCtx(ctx, time.Until(until)) {
				return nil, ctx.Err()
			}
			continue
		}

		active := nodes.ActiveOrder(ids.Session)
		if g.cfg.Failover.MultiAccount {
			active = nodes.ActiveOrderFair()
		}
		if len(active) == 0 {
			if earliest := nodes.EarliestCooldown(); !earliest.IsZero() && !earliest.After(deadline) {
				g.logger.Debug("all keys cooling down, holding request", "request_id", ids.Request, "tier", route.Tier, "wait_seconds", int(time.Until(earliest).Seconds()))
				if !sleepCtx(ctx, time.Until(earliest)) {
					return nil, ctx.Err()
				}
				continue
			}
			if anyQuota {
				return nil, errQuotaExhausted
			}
			return nil, errAllCooling
		}

		// Every active node becomes a silent failover candidate. Quota-exhausted
		// keys drop out of the rotation immediately (COOLING_DOWN) without ever
		// reaching the client.
		for _, node := range active {
			proxy := nodes.Proxy(node)
			if proxy == nil {
				lastErr = errors.New("upstream key has no proxy binding")
				continue
			}
			endpoint := strings.TrimRight(baseURL, "/") + protocolPath(route.Protocol, g.cfg.UpstreamMode)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if g.cfg.UpstreamMode.isOpenAI() {
				req.Header.Set("User-Agent", "opencode2api/1.0")
			} else {
				req.Header.Set("User-Agent", opencodeUserAgent())
				req.Header.Set("x-opencode-client", "cli")
				req.Header.Set("x-opencode-session", ids.Session)
				req.Header.Set("x-opencode-request", ids.Request)
				req.Header.Set("x-opencode-project", ids.Project)
				if node.machineID != "" {
					req.Header.Set("x-machine-id", node.machineID)
					req.Header.Set("vscode-machine-id", node.vscodeMachine)
				}
			}
			if route.Protocol == ProtocolAnthropic {
				req.Header.Set("x-api-key", node.key)
				req.Header.Set("anthropic-version", "2023-06-01")
			} else {
				req.Header.Set("Authorization", "Bearer "+node.key)
			}

			resp, err := proxy.client.Do(req)
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			proxyFailed := g.syncProxyResult(ctx, proxy, status, err)

			if err == nil && resp.StatusCode/100 == 2 {
				nodes.MarkSuccess(node)
				nodes.ClearAccountThrottle()
				if g.cfg.RateLimit.Enabled {
					nodes.ObserveRateLimit(node, resp.Header, g.cfg.RateLimit.Proactive, g.cfg.RateLimit.RotateAtRemaining)
				}
				g.logger.Debug("upstream accepted request", "request_id", ids.Request, "tier", route.Tier, "proxy", redactURL(proxy.name))
				return resp, nil
			}

			// Non-2xx responses must be inspected before deciding to fail over.
			if err == nil {
				captured := captureBody(resp, 4<<20)
				// Restore the body so callers that receive this response
				// (4xx passthrough, lastResponse fallback) can still read it.
				resp.Body = io.NopCloser(bytes.NewReader(captured.body))
				// The go tier is the last-resort fallback. A long quota-park,
				// account-reject, or throttle window on its single key would
				// knock the safety net offline for minutes, so it only ever
				// takes the short per-failure cooldown and keeps retrying.
				isFallback := route.Tier == TierGo
				if isQuotaError(captured.status, captured.body, g.cfg.Failover, g.cfg.UpstreamMode) {
					if !isFallback && g.cfg.Failover.MultiAccount && g.cfg.Failover.QuotaParkMinutes > 0 {
						// Daily free-quota cap: park the account out of rotation
						// for the quota window instead of the short cooldown, so
						// it stops wasting the remaining accounts' retries.
						park := time.Duration(g.cfg.Failover.QuotaParkMinutes) * time.Minute
						nodes.MarkQuotaParked(node, park)
						g.logger.Warn("upstream account quota exhausted, parking until quota window", "request_id", ids.Request, "tier", route.Tier, "status", captured.status, "park_minutes", g.cfg.Failover.QuotaParkMinutes, "proxy", redactURL(proxy.name))
					} else if isFallback {
						nodes.MarkFailure(node, resp, err)
						g.logger.Warn("fallback upstream quota error, short cooldown only", "request_id", ids.Request, "tier", route.Tier, "status", captured.status, "proxy", redactURL(proxy.name))
					} else {
						nodes.MarkQuotaExceeded(node, quotaCooldown)
						g.logger.Warn("upstream quota exceeded, silent failover", "request_id", ids.Request, "tier", route.Tier, "status", captured.status, "proxy", redactURL(proxy.name))
					}
					anyQuota = true
					continue
				}
				// Account-level rejections (bad key, no payment method) are
				// stable conditions: cool the key for a long period so the
				// remaining keys carry the traffic, and try the next key
				// silently. The last rejected response is kept as a fallback
				// so the client still gets the real error when every key is
				// rejected. The fallback tier uses a short cooldown instead so
				// a transient relay 401/403 cannot disable it.
				if captured.status == http.StatusUnauthorized || captured.status == http.StatusForbidden {
					if isFallback {
						nodes.MarkFailure(node, resp, err)
					} else {
						nodes.MarkAccountRejected(node, accountRejectCooldown)
					}
					lastResponse = resp
					g.logger.Warn("upstream rejected key, silent failover", "request_id", ids.Request, "tier", route.Tier, "status", captured.status, "proxy", redactURL(proxy.name))
					continue
				}
				// A 429 cools this key. In single-account mode, distinct keys of
				// the same pool hitting 429 inside the shared window prove the
				// upstream is rate limiting the whole account: key rotation
				// cannot help, so the pool enters a throttle window and the
				// request is held (backpressure) until it elapses. In
				// multi-account mode each key is its own account, so a 429
				// throttles only that key and the others keep serving. For the
				// fallback tier a 429 is a short cooldown: the single key must
				// stay in rotation.
				if captured.status == http.StatusTooManyRequests {
					if isFallback {
						nodes.MarkFailure(node, resp, err)
						lastResponse = resp
						g.logger.Debug("fallback upstream rate limited, short cooldown only", "request_id", ids.Request, "tier", route.Tier, "proxy", redactURL(proxy.name))
						continue
					}
					if g.cfg.Failover.MultiAccount {
						nodes.MarkFailure(node, resp, err)
						nodes.MarkNodeThrottled(node, g.cfg.Failover.Throttle)
						g.logger.Debug("account rate limited, cooling account only", "request_id", ids.Request, "tier", route.Tier, "proxy", redactURL(proxy.name))
						// Re-evaluate: the throttled account drops out of rotation
						// and, when every account is limited, the pool holds until
						// the earliest throttle window elapses (backpressure) and
						// probes once, exactly like the pooled throttle path.
						continue requestLoop
					}
					nodes.MarkFailure(node, resp, err)
					if nodes.Record429(node, sharedWindow, g.cfg.Failover.Throttle.Shared429Threshold) {
						nodes.MarkAccountThrottled(g.cfg.Failover.Throttle)
						g.logger.Warn("shared account rate limit detected, throttling tier", "request_id", ids.Request, "tier", route.Tier, "proxy", redactURL(proxy.name))
						continue requestLoop
					}
					lastResponse = resp
					continue
				}
				if captured.status >= 400 && captured.status < 500 {
					nodes.MarkSuccess(node)
					g.logger.Debug("upstream rejected request without retry", "request_id", ids.Request, "status", captured.status, "proxy", redactURL(proxy.name))
					return resp, nil
				}
				lastResponse = resp
			} else {
				lastErr = err
			}

			if proxyFailed {
				if nodes.Proxy(node) == proxy {
					nodes.MarkFailure(node, resp, err)
				}
			} else {
				nodes.MarkFailure(node, resp, err)
			}
			if err != nil {
				g.logger.Debug("upstream transport error", "request_id", ids.Request, "tier", route.Tier, "proxy", redactURL(proxy.name), "error", err)
			} else {
				g.logger.Debug("upstream rejected request", "request_id", ids.Request, "status", resp.StatusCode, "proxy", redactURL(proxy.name), "tier", route.Tier)
			}
		}
		// A full pass over the pool failed. Transient upstream failures
		// (502/503/504, transport errors) justify another pass once key
		// cooldowns elapse — vital for single-key tiers with no failover
		// target. The loop head waits out cooldowns; the request context
		// bounds total time.
		attempt++
		if attempt < maxAttempts && transientRetryable(lastResponse, lastErr) {
			g.logger.Debug("transient upstream failure, retrying pool pass", "request_id", ids.Request, "tier", route.Tier, "attempt", attempt, "max_attempts", maxAttempts)
			continue requestLoop
		}
		break
	}

	if lastResponse != nil {
		return lastResponse, nil
	}
	if anyQuota {
		return nil, errQuotaExhausted
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errAllCooling
}

// throttleError carries the upstream rate-limit window so the gateway can
// answer 503 with a Retry-After header instead of leaving the client guessing.
type throttleError struct {
	retryAfter time.Duration
}

// transientRetryable reports whether a failed pool pass is worth retrying:
// transport errors and upstream 502/503/504 are treated as transient; request
// shape errors (4xx) and quota/limit paths are not (they have their own flows).
func transientRetryable(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	if resp == nil {
		return false
	}
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func (e *throttleError) Error() string {
	return fmt.Sprintf("upstream account rate limited, retry after %s", e.retryAfter.Round(time.Second))
}

// quotaRetryAfter returns whole seconds until the tier's keys are expected to
// be available again, so a 503 from every-key-quota carries a useful
// Retry-After instead of a bare 503.
func (g *Gateway) quotaRetryAfter(tier Tier) int {
	nodes := g.zenNodes
	if tier == TierGo {
		nodes = g.goNodes
	}
	if nodes == nil {
		return 0
	}
	earliest := nodes.EarliestBusy()
	if earliest.IsZero() {
		return 0
	}
	seconds := int(time.Until(earliest).Seconds()) + 1
	if seconds < 1 {
		return 1
	}
	return seconds
}

// sleepCtx sleeps for d or until ctx is done, reporting whether it completed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// errQuotaExhausted signals that every account key in the pool is cooling down
// after hitting upstream quota limits.
var errQuotaExhausted = errors.New("all upstream keys exhausted their quota")

// errAllCooling signals that every key is cooling down or unavailable for a
// reason other than quota (rate limits, account rejections, transport errors).
var errAllCooling = errors.New("all upstream keys cooling down or unavailable")

// accountRejectCooldown cools keys rejected with 401/403 (invalid key, no
// payment method). The condition is stable, so a long cooldown keeps the key
// out of rotation instead of wasting a request on it every retry.
const accountRejectCooldown = 10 * time.Minute

// syncProxyResult updates proxy health from real traffic. Only timeouts and
// connection refusals mark a proxy unavailable. Other errors and 4xx/5xx
// responses trigger a neutral URL check without being treated as proxy failure.
func (g *Gateway) syncProxyResult(ctx context.Context, proxy *proxyTransport, status int, err error) bool {
	if proxy == nil {
		return false
	}
	if isProxyFailure(err) {
		// A direct (no-proxy) egress cannot itself be "unavailable": an upstream
		// timeout/refusal is the upstream's condition, not the route's. Marking
		// it unhealthy would disable every key bound to it — including the
		// fallback tier — whenever one upstream is merely slow or down.
		if proxy.name != "direct" {
			g.rebindFailedProxy(proxy)
		}
		g.verifyProxyAfterError(ctx, proxy, status)
		return true
	}
	if status >= 200 && status < 400 {
		wasHealthy := proxy.healthy.Swap(true)
		if !wasHealthy {
			g.restoreProxy(proxy)
		}
		return false
	}
	if err != nil {
		g.verifyProxyAfterError(ctx, proxy, status)
		return false
	}
	if status >= 400 && status < 600 {
		g.verifyProxyAfterError(ctx, proxy, status)
	}
	return false
}

func (g *Gateway) verifyProxyAfterError(ctx context.Context, proxy *proxyTransport, status int) {
	if !proxy.checking.CompareAndSwap(false, true) {
		return
	}
	// The client request may finish or be cancelled while the verification is
	// running. Keep its values but give the proxy check an independent timeout.
	checkCtx := context.WithoutCancel(ctx)
	go func() {
		result := g.transports.checkClaimedProxy(checkCtx, proxy, proxyHealthCheckURL, proxyHealthCheckTimeout)
		g.applyProxyHealthResult(result, "upstream HTTP response", status)
	}()
}

func (g *Gateway) rebindFailedProxy(proxy *proxyTransport) (zenMoved, goMoved int) {
	if proxy == nil {
		return 0, 0
	}
	wasHealthy := proxy.healthy.Swap(false)
	return g.rebindUnavailableProxy(proxy, wasHealthy)
}

func (g *Gateway) rebindUnavailableProxy(proxy *proxyTransport, wasHealthy bool) (zenMoved, goMoved int) {
	zenMoved = g.zenNodes.RebindProxy(proxy.index)
	goMoved = g.goNodes.RebindProxy(proxy.index)
	if wasHealthy || zenMoved+goMoved > 0 {
		g.logger.Warn("proxy unavailable", "proxy", redactURL(proxy.name), "zen_keys_moved", zenMoved, "go_keys_moved", goMoved)
	}
	return zenMoved, goMoved
}

func (g *Gateway) restoreProxy(proxy *proxyTransport) (zenMoved, goMoved int) {
	if proxy == nil {
		return 0, 0
	}
	zenMoved = g.zenNodes.RestoreProxy(proxy.index)
	goMoved = g.goNodes.RestoreProxy(proxy.index)
	if zenMoved+goMoved > 0 {
		g.logger.Info("proxy restored", "proxy", redactURL(proxy.name), "zen_keys_moved", zenMoved, "go_keys_moved", goMoved)
	}
	return zenMoved, goMoved
}

func (g *Gateway) StartProxyHealthChecks(ctx context.Context) {
	check := func() {
		results := g.transports.CheckHealth(ctx, proxyHealthCheckURL, proxyHealthCheckTimeout)
		for _, result := range results {
			g.applyProxyHealthResult(result, "scheduled health check", 0)
		}
	}
	go func() {
		ticker := time.NewTicker(proxyHealthCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check()
			}
		}
	}()
}

func (g *Gateway) applyProxyHealthResult(result proxyHealthResult, source string, upstreamStatus int) {
	// A direct egress has no intermediary that can go "unavailable"; an upstream
	// or probe outage must never disable the route (and with it the fallback
	// tier). Only real proxies can be marked unhealthy.
	if result.proxy != nil && result.proxy.name == "direct" {
		if result.err == nil {
			result.proxy.healthy.Store(true)
		}
		return
	}
	if result.err == nil {
		if !result.wasHealthy {
			g.restoreProxy(result.proxy)
		}
		g.logger.Debug("proxy health check passed", "source", source, "upstream_status", upstreamStatus, "proxy", redactURL(result.proxy.name))
		return
	}
	if !result.failed {
		g.logger.Debug("proxy health check inconclusive", "source", source, "upstream_status", upstreamStatus, "proxy", redactURL(result.proxy.name), "error", result.err)
		return
	}
	if g.transports.hasHealthy() {
		zenMoved, goMoved := g.rebindUnavailableProxy(result.proxy, result.wasHealthy)
		if result.wasHealthy || zenMoved+goMoved > 0 {
			g.logger.Warn("proxy health check failed", "source", source, "upstream_status", upstreamStatus, "proxy", redactURL(result.proxy.name), "zen_keys_moved", zenMoved, "go_keys_moved", goMoved, "error", result.err)
			return
		}
	}
	g.logger.Debug("proxy health check still failing", "source", source, "upstream_status", upstreamStatus, "proxy", redactURL(result.proxy.name), "error", result.err)
}

func protocolPath(protocol Protocol, mode UpstreamMode) string {
	if mode.isOpenAI() {
		// OpenAI-compatible upstreams (Gemini) expose chat under /chat/completions
		// on the configured base URL; /responses and /messages are not supported.
		if protocol == ProtocolResponses {
			return "/responses"
		}
		if protocol == ProtocolAnthropic {
			return "/messages"
		}
		return "/chat/completions"
	}
	switch protocol {
	case ProtocolResponses:
		return "/v1/responses"
	case ProtocolAnthropic:
		return "/v1/messages"
	default:
		return "/v1/chat/completions"
	}
}

func (g *Gateway) StartModelRefresh(ctx context.Context) {
	refresh := func() {
		if g.cfg.UpstreamMode.isOpenAI() && (len(g.cfg.Models.Static) > 0 || len(g.cfg.Models.StaticGo) > 0) {
			// OpenAI-compatible upstreams may not expose an OpenAI-shaped
			// /models endpoint, so the catalog is taken from the configured
			// static lists: Static routes to the primary upstream (zen tier),
			// StaticGo to the second upstream (go tier).
			g.catalog.Replace(g.cfg.Models.Static, g.cfg.Models.StaticGo)
			g.logger.Info("model catalog loaded from static list", "models", len(g.cfg.Models.Static)+len(g.cfg.Models.StaticGo))
			return
		}
		var zen, goModels []string
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); zen = g.refreshTier(ctx, g.cfg.Upstream.Zen, g.zenNodes) }()
		go func() { defer wg.Done(); goModels = g.refreshTier(ctx, g.cfg.Upstream.Go, g.goNodes) }()
		wg.Wait()
		if zen != nil || goModels != nil {
			g.catalog.Replace(zen, goModels)
			g.logger.Info("model catalog refreshed", "models", len(g.catalog.List()))
		}
	}
	go func() {
		refresh()
		ticker := time.NewTicker(time.Duration(g.cfg.Models.RefreshSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
}

func (g *Gateway) refreshTier(ctx context.Context, base string, nodes *nodePool) []string {
	cursor := nodes.Cursor()
	for attempt := 0; attempt < g.cfg.Retry.MaxAttempts; attempt++ {
		node := cursor.Next()
		if node == nil {
			return nil
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		proxy := nodes.Proxy(node)
		if proxy == nil {
			cancel()
			return nil
		}
		models, status, err := fetchModels(refreshCtx, proxy.client, base, node.key, g.cfg.UpstreamMode)
		g.syncProxyResult(refreshCtx, proxy, status, err)
		cancel()
		if err == nil {
			nodes.MarkSuccess(node)
			return models
		}
		nodes.MarkFailure(node, nil, err)
		g.logger.Debug("model refresh attempt failed", "upstream", base, "attempt", attempt+1, "error", err)
	}
	g.logger.Warn("model refresh failed", "upstream", base)
	return nil
}

func copyErrorResponse(w http.ResponseWriter, protocol Protocol, resp *http.Response, requestID string) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	message := http.StatusText(resp.StatusCode)
	var value map[string]any
	if json.Unmarshal(body, &value) == nil {
		message = firstString(stringAt(value, "error", "message"), stringAt(value, "message"), message)
	}
	writeAPIError(w, protocol, resp.StatusCode, message, "upstream_error", requestID)
}

func writeAPIError(w http.ResponseWriter, protocol Protocol, status int, message, kind, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	if requestID != "" {
		w.Header().Set("x-request-id", requestID)
	}
	if protocol == ProtocolAnthropic {
		writeJSONStatus(w, status, map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message}})
		return
	}
	writeJSONStatus(w, status, map[string]any{"error": map[string]any{"message": message, "type": kind, "param": nil, "code": nil}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	writeJSONStatus(w, status, value)
}

func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func recoveryMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				logger.Error("request panic", "error", value)
				writeAPIError(w, ProtocolChat, http.StatusInternalServerError, "internal server error", "server_error", "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
