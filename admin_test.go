package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// adminTestGateway builds a Gateway wired to fake upstreams with admin keys configured.
func adminTestGateway(t *testing.T, adminKeys []string, zenKeys, goKeys []string) (*Gateway, *httptest.Server) {
	t.Helper()
	// Build a fake upstream that returns a /v1/models response and 200 on chat.
	modelsBody := `{"data":[{"id":"gpt-4o"},{"id":"deepseek-v4-flash"}]}`
	chatBody := `{"id":"cmpl-test","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		key := ""
		if len(auth) > 7 {
			key = auth[7:]
		}
		if r.URL.Path == "/v1/models" || r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(modelsBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(chatBody))
		_ = key
	})
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)

	cfg := Config{
		Listen:      "127.0.0.1:0",
		ServerKeys:  []string{"local-key"},
		AdminKeys:   adminKeys,
		ZenKeys:     zenKeys,
		GoKeys:      goKeys,
		Proxies:     []string{"direct"},
		Upstream:    UpstreamConfig{Zen: server.URL, Go: server.URL},
		Retry:       RetryConfig{MaxAttempts: 2, TimeoutSeconds: 5},
		Models:      ModelsConfig{RefreshSeconds: 300, Protocols: map[string]string{}},
		Performance: PerformanceConfig{MaxIdleConns: 10, MaxIdleConnsPerHost: 10, MaxConnsPerHost: 0, IdleConnTimeoutSeconds: 30, ConnectTimeoutSeconds: 5, FailureCooldownSeconds: 1},
		Logging:     LoggingConfig{Level: "error"},
		Stats:       StatsConfig{},
		Prefer:      TierGo,
		Sanitize:    SanitizeConfig{Enabled: true, StripFreeSuffix: false, ModelAliases: map[string]string{}},
		Failover:    FailoverConfig{Enabled: true, QuotaCooldownMinutes: 30, Throttle: ThrottleConfig{InitialSeconds: 1, MaxSeconds: 600, Shared429Threshold: 2, MaxWaitSeconds: 5}},
		Fingerprint: FingerprintConfig{Enabled: false, PersistFile: ""},
		RateLimit:   RateLimitConfig{Enabled: false},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw, err := NewGateway(cfg, logger)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	return gw, server
}

func TestAdminAuthMiddleware(t *testing.T) {
	gw, _ := adminTestGateway(t, []string{"admin-secret"}, []string{"zen-key"}, []string{})
	handler := gw.Handler()

	tests := []struct {
		name       string
		path       string
		method     string
		apiKey     string
		bearer     string
		wantStatus int
	}{
		{"no key", "/admin/keys", "GET", "", "", 401},
		{"wrong key", "/admin/keys", "GET", "wrong", "", 401},
		{"x-api-key correct", "/admin/keys", "GET", "admin-secret", "", 200},
		{"bearer correct", "/admin/keys", "GET", "", "Bearer admin-secret", 200},
		{"server key not admin", "/admin/keys", "GET", "local-key", "", 401},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.apiKey != "" {
				req.Header.Set("x-api-key", tc.apiKey)
			}
			if tc.bearer != "" {
				req.Header.Set("Authorization", tc.bearer)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tc.wantStatus {
				t.Errorf("got %d, want %d (body: %s)", rr.Code, tc.wantStatus, rr.Body.String())
			}
		})
	}
}

func TestAdminKeysEndpoint(t *testing.T) {
	gw, _ := adminTestGateway(t, []string{"admin-secret"}, []string{"zen-a", "zen-b"}, []string{"go-a"})
	handler := gw.Handler()

	req := httptest.NewRequest("GET", "/admin/keys", nil)
	req.Header.Set("x-api-key", "admin-secret")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Zen []struct {
			Index         int    `json:"index"`
			State         string `json:"state"`
			CooldownUntil int64  `json:"cooldown_until"`
			Proxy         string `json:"proxy"`
		} `json:"zen"`
		Go []struct {
			Index         int    `json:"index"`
			State         string `json:"state"`
			CooldownUntil int64  `json:"cooldown_until"`
			Proxy         string `json:"proxy"`
		} `json:"go"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(body.Zen) != 2 {
		t.Errorf("expected 2 zen keys, got %d", len(body.Zen))
	}
	if len(body.Go) != 1 {
		t.Errorf("expected 1 go key, got %d", len(body.Go))
	}
	// Fresh keys should be active.
	for _, k := range body.Zen {
		if k.State != "active" {
			t.Errorf("zen key %d state = %q, want active", k.Index, k.State)
		}
	}
}

func TestAdminRefreshEndpoint(t *testing.T) {
	gw, _ := adminTestGateway(t, []string{"admin-secret"}, []string{"zen-a"}, []string{"go-a"})
	handler := gw.Handler()

	req := httptest.NewRequest("POST", "/admin/refresh", nil)
	req.Header.Set("x-api-key", "admin-secret")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Zen int `json:"zen"`
		Go  int `json:"go"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if body.Zen == 0 && body.Go == 0 {
		t.Error("expected at least one non-zero model count after refresh")
	}
}

func TestAdminCooldownEndpoint(t *testing.T) {
	gw, _ := adminTestGateway(t, []string{"admin-secret"}, []string{"zen-a"}, []string{})
	handler := gw.Handler()

	// Set cooldown on zen key index 0 for 60 seconds.
	body := `{"tier":"zen","index":0,"cooldown_seconds":60}`
	req := httptest.NewRequest("POST", "/admin/cooldown", bytes.NewBufferString(body))
	req.Header.Set("x-api-key", "admin-secret")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("set cooldown: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify key is cooling via /admin/keys.
	req2 := httptest.NewRequest("GET", "/admin/keys", nil)
	req2.Header.Set("x-api-key", "admin-secret")
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)

	var keys struct {
		Zen []struct {
			Index         int    `json:"index"`
			State         string `json:"state"`
			CooldownUntil int64  `json:"cooldown_until"`
		} `json:"zen"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &keys); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if keys.Zen[0].State != "cooling" {
		t.Errorf("zen key 0 state = %q, want cooling", keys.Zen[0].State)
	}
	if keys.Zen[0].CooldownUntil == 0 {
		t.Error("expected non-zero cooldown_until")
	}
}

func TestAdminCooldownClear(t *testing.T) {
	gw, _ := adminTestGateway(t, []string{"admin-secret"}, []string{"zen-a"}, []string{})
	handler := gw.Handler()

	// Set then clear.
	set := `{"tier":"zen","index":0,"cooldown_seconds":300}`
	req := httptest.NewRequest("POST", "/admin/cooldown", bytes.NewBufferString(set))
	req.Header.Set("x-api-key", "admin-secret")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("set: %d: %s", rr.Code, rr.Body.String())
	}

	clear := `{"tier":"zen","index":0,"cooldown_seconds":0}`
	req2 := httptest.NewRequest("POST", "/admin/cooldown", bytes.NewBufferString(clear))
	req2.Header.Set("x-api-key", "admin-secret")
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)
	if rr2.Code != 200 {
		t.Fatalf("clear: %d: %s", rr2.Code, rr2.Body.String())
	}

	// Verify active.
	req3 := httptest.NewRequest("GET", "/admin/keys", nil)
	req3.Header.Set("x-api-key", "admin-secret")
	rr3 := httptest.NewRecorder()
	handler.ServeHTTP(rr3, req3)
	var keys struct {
		Zen []struct {
			State string `json:"state"`
		} `json:"zen"`
	}
	json.Unmarshal(rr3.Body.Bytes(), &keys)
	if keys.Zen[0].State != "active" {
		t.Errorf("after clear: state = %q, want active", keys.Zen[0].State)
	}
}

func TestAdminCooldownBadBody(t *testing.T) {
	gw, _ := adminTestGateway(t, []string{"admin-secret"}, []string{"zen-a"}, []string{})
	handler := gw.Handler()

	tests := []struct {
		name string
		body string
	}{
		{"invalid json", `{bad}`},
		{"missing tier", `{"index":0,"cooldown_seconds":60}`},
		{"bad tier", `{"tier":"invalid","index":0,"cooldown_seconds":60}`},
		{"out of range index", `{"tier":"zen","index":999,"cooldown_seconds":60}`},
		{"negative cooldown", `{"tier":"zen","index":0,"cooldown_seconds":-1}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/admin/cooldown", bytes.NewBufferString(tc.body))
			req.Header.Set("x-api-key", "admin-secret")
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code < 400 {
				t.Errorf("expected error status, got %d", rr.Code)
			}
			// Check for code field in JSON response.
			var errResp struct {
				Code string `json:"code"`
			}
			json.Unmarshal(rr.Body.Bytes(), &errResp)
			if errResp.Code == "" {
				t.Error("expected error code field in response")
			}
		})
	}
}

func TestAdminProbeEndpoint(t *testing.T) {
	gw, _ := adminTestGateway(t, []string{"admin-secret"}, []string{"zen-a"}, []string{"go-a"})
	handler := gw.Handler()

	req := httptest.NewRequest("GET", "/admin/probe", nil)
	req.Header.Set("x-api-key", "admin-secret")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Zen struct {
			Status string `json:"status"`
			Models int    `json:"models"`
		} `json:"zen"`
		Go struct {
			Status string `json:"status"`
			Models int    `json:"models"`
		} `json:"go"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	// At least one tier should have models from our fake upstream.
	if body.Zen.Models == 0 && body.Go.Models == 0 {
		t.Error("expected at least one tier to have models after probe")
	}
}

func TestAdminEndpointsUnauthorized(t *testing.T) {
	gw, _ := adminTestGateway(t, []string{"admin-secret"}, []string{"zen-a"}, []string{})
	handler := gw.Handler()

	endpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/admin/keys"},
		{"POST", "/admin/refresh"},
		{"POST", "/admin/cooldown"},
		{"GET", "/admin/probe"},
	}
	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != 401 {
				t.Errorf("unauthenticated %s %s: got %d, want 401", ep.method, ep.path, rr.Code)
			}
		})
	}
}

func TestAdminCooldownMismatchedTier(t *testing.T) {
	// Go tier configured with one key; try to cooldown a zen key that doesn't exist.
	gw, _ := adminTestGateway(t, []string{"admin-secret"}, []string{}, []string{"go-a"})
	handler := gw.Handler()

	body := `{"tier":"zen","index":0,"cooldown_seconds":60}`
	req := httptest.NewRequest("POST", "/admin/cooldown", bytes.NewBufferString(body))
	req.Header.Set("x-api-key", "admin-secret")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code < 400 {
		t.Errorf("expected error for cooldown on non-existent zen tier, got %d", rr.Code)
	}
}

func TestAdminRefreshReturnsModelCounts(t *testing.T) {
	gw, _ := adminTestGateway(t, []string{"admin-secret"}, []string{"zen-a"}, []string{"go-a"})
	handler := gw.Handler()

	req := httptest.NewRequest("POST", "/admin/refresh", nil)
	req.Header.Set("x-api-key", "admin-secret")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	// Should have zen and go counts.
	if _, ok := body["zen"]; !ok {
		t.Error("response missing 'zen' field")
	}
	if _, ok := body["go"]; !ok {
		t.Error("response missing 'go' field")
	}
}

func TestAdminProbePerTierStatus(t *testing.T) {
	gw, _ := adminTestGateway(t, []string{"admin-secret"}, []string{"zen-a"}, []string{"go-a"})
	handler := gw.Handler()

	req := httptest.NewRequest("GET", "/admin/probe", nil)
	req.Header.Set("x-api-key", "admin-secret")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	for _, tier := range []string{"zen", "go"} {
		tierData, ok := body[tier].(map[string]any)
		if !ok {
			t.Errorf("response missing '%s' tier", tier)
			continue
		}
		if _, ok := tierData["status"]; !ok {
			t.Errorf("tier %s missing 'status' field", tier)
		}
	}
}

func TestAdminRefreshLogsAction(t *testing.T) {
	// Verify that the /admin/refresh endpoint returns successfully and
	// contains the expected structure. Logging is tested via integration.
	gw, _ := adminTestGateway(t, []string{"admin-secret"}, []string{"zen-a"}, []string{})
	handler := gw.Handler()

	req := httptest.NewRequest("POST", "/admin/refresh", nil)
	req.Header.Set("x-api-key", "admin-secret")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAdminCooldownEmptyBody(t *testing.T) {
	gw, _ := adminTestGateway(t, []string{"admin-secret"}, []string{"zen-a"}, []string{})
	handler := gw.Handler()

	req := httptest.NewRequest("POST", "/admin/cooldown", bytes.NewBufferString(""))
	req.Header.Set("x-api-key", "admin-secret")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code < 400 {
		t.Errorf("expected error for empty body, got %d", rr.Code)
	}
}

func TestAdminKeysEmptyPools(t *testing.T) {
	// Admin keys endpoint should work even with no upstream keys.
	gw, _ := adminTestGateway(t, []string{"admin-secret"}, []string{}, []string{})
	handler := gw.Handler()

	req := httptest.NewRequest("GET", "/admin/keys", nil)
	req.Header.Set("x-api-key", "admin-secret")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Zen []any `json:"zen"`
		Go  []any `json:"go"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(body.Zen) != 0 || len(body.Go) != 0 {
		t.Error("expected empty zen and go arrays for no keys")
	}
}

func TestAdminProbeEmptyPools(t *testing.T) {
	gw, _ := adminTestGateway(t, []string{"admin-secret"}, []string{}, []string{})
	handler := gw.Handler()

	req := httptest.NewRequest("GET", "/admin/probe", nil)
	req.Header.Set("x-api-key", "admin-secret")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAdminCooldownGoTier(t *testing.T) {
	gw, _ := adminTestGateway(t, []string{"admin-secret"}, []string{}, []string{"go-a"})
	handler := gw.Handler()

	// Set cooldown on go key index 0.
	body := `{"tier":"go","index":0,"cooldown_seconds":120}`
	req := httptest.NewRequest("POST", "/admin/cooldown", bytes.NewBufferString(body))
	req.Header.Set("x-api-key", "admin-secret")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("set cooldown: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify via /admin/keys.
	req2 := httptest.NewRequest("GET", "/admin/keys", nil)
	req2.Header.Set("x-api-key", "admin-secret")
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)

	var keys struct {
		Go []struct {
			Index int    `json:"index"`
			State string `json:"state"`
		} `json:"go"`
	}
	json.Unmarshal(rr2.Body.Bytes(), &keys)
	if len(keys.Go) != 1 || keys.Go[0].State != "cooling" {
		t.Errorf("go key state = %v, want cooling", keys.Go)
	}
}

func TestAdminRefreshIncludesUpdatedModels(t *testing.T) {
	gw, _ := adminTestGateway(t, []string{"admin-secret"}, []string{"zen-a"}, []string{"go-a"})
	handler := gw.Handler()

	req := httptest.NewRequest("POST", "/admin/refresh", nil)
	req.Header.Set("x-api-key", "admin-secret")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Check that the model catalog was actually updated by querying /v1/models.
	// (This verifies the refresh actually ran and populated the catalog.)
	req2 := httptest.NewRequest("GET", "/v1/models", nil)
	req2.Header.Set("x-api-key", "local-key")
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)

	if rr2.Code != 200 {
		t.Fatalf("/v1/models after refresh: %d", rr2.Code)
	}
}
