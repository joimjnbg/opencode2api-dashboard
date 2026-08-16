package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"
)

// testGatewayCfg builds a Gateway wired to a fake upstream, mutating the
// default config before construction. testGateway keeps its fixed config;
// multi-account tests use this builder.
func testGatewayCfg(t *testing.T, keyAnswers map[string]int, fingerprintEnabled bool, mutate func(*Config)) (*Gateway, *fakeUpstream) {
	t.Helper()
	upstream := newFakeUpstream(keyAnswers)
	server := httptest.NewServer(upstream.handler())
	t.Cleanup(server.Close)

	cfg := Config{
		Listen:      "127.0.0.1:0",
		ServerKeys:  []string{"local"},
		ZenKeys:     []string{"key-a", "key-b"},
		GoKeys:      []string{},
		Proxies:     []string{"direct"},
		Upstream:    UpstreamConfig{Zen: server.URL, Go: server.URL},
		Retry:       RetryConfig{MaxAttempts: 3, TimeoutSeconds: 30},
		Models:      ModelsConfig{RefreshSeconds: 300, Protocols: map[string]string{}},
		Performance: PerformanceConfig{MaxIdleConns: 10, MaxIdleConnsPerHost: 10, MaxConnsPerHost: 0, IdleConnTimeoutSeconds: 30, ConnectTimeoutSeconds: 5, FailureCooldownSeconds: 1},
		Logging:     LoggingConfig{Level: "error"},
		Stats:       StatsConfig{},
		Prefer:      TierGo,
		Sanitize:    SanitizeConfig{Enabled: true, StripFreeSuffix: true, ModelAliases: map[string]string{}},
		Failover:    FailoverConfig{Enabled: true, QuotaCooldownMinutes: 30, TreatGeneric429AsQuota: false, Throttle: ThrottleConfig{InitialSeconds: 1, MaxSeconds: 600, Shared429Threshold: 2, MaxWaitSeconds: 5}},
		Fingerprint: FingerprintConfig{Enabled: fingerprintEnabled, PersistFile: ""},
		RateLimit:   RateLimitConfig{Enabled: true, Proactive: true, RotateAtRemaining: 2},
	}
	mutate(&cfg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw, err := NewGateway(cfg, logger)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	return gw, upstream
}

// TestPerAccount429DoesNotThrottlePool ensures a 429 on one account in
// multi-account mode cools only that account: the other account keeps serving
// and no pool-wide throttle window opens.
func TestPerAccount429DoesNotThrottlePool(t *testing.T) {
	gw, upstream := testGatewayCfg(t, map[string]int{}, true, func(c *Config) {
		c.Failover.MultiAccount = true
	})
	// First observed request (key-a, rotation start) returns a generic 429
	// (rate limit, not quota), then all keys succeed.
	upstream.rateLimitFirst.Store(1)
	body := []byte(`{"model":"deepseek-v4-flash","messages":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ids := requestIDs{Session: "s", Request: "r", Project: "p"}

	// First request: key-a 429s, key-b must serve without a pool throttle.
	resp, err := gw.doUpstream(ctx, catalogTestRoute(), body, ids)
	if err != nil {
		t.Fatalf("doUpstream: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("client received HTTP %d, want 200", resp.StatusCode)
	}
	if !gw.zenNodes.ThrottleDeadline().IsZero() {
		t.Error("pool-wide throttle must NOT be active after a single account 429s")
	}
	foundThrottled := false
	for _, st := range gw.zenNodes.StatusSnapshot() {
		if st.State == "throttled" {
			foundThrottled = true
		}
	}
	if !foundThrottled {
		t.Errorf("expected an account to report throttled, got %+v", gw.zenNodes.StatusSnapshot())
	}

	// A second request must not wait out a pool throttle window.
	start := time.Now()
	resp2, err := gw.doUpstream(ctx, catalogTestRoute(), body, requestIDs{Session: "s2", Request: "r2", Project: "p"})
	if err != nil {
		t.Fatalf("second doUpstream: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("second request got HTTP %d, want 200", resp2.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Errorf("second request waited %v: a pool throttle must not delay it", elapsed)
	}

	if seen := upstream.PerKeyCounts(); seen["key-a"] == 0 {
		t.Errorf("key-a should have received the failing request: %v", seen)
	}
}

// TestFairRotationSpreadsAcrossAccounts verifies multi-account mode spreads
// consecutive requests across accounts instead of pinning them to one.
func TestFairRotationSpreadsAcrossAccounts(t *testing.T) {
	gw, upstream := testGatewayCfg(t, map[string]int{}, true, func(c *Config) {
		c.Failover.MultiAccount = true
	})
	body := []byte(`{"model":"deepseek-v4-flash","messages":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 0; i < 8; i++ {
		ids := requestIDs{Session: "same-session", Request: "r", Project: "p"}
		resp, err := gw.doUpstream(ctx, catalogTestRoute(), body, ids)
		if err != nil {
			t.Fatalf("request %d: doUpstream: %v", i, err)
		}
		resp.Body.Close()
	}
	perKey := upstream.PerKeyCounts()
	if perKey["key-a"] == 0 {
		t.Errorf("key-a was never used under fair rotation: %v", perKey)
	}
	if perKey["key-b"] == 0 {
		t.Errorf("key-b was never used under fair rotation: %v", perKey)
	}
}

// TestMultiAccountConversationContinuesAcrossAccounts ensures a single request
// keeps succeeding across accounts on silent failover within the loop.
func TestMultiAccountConversationContinuesAcrossAccounts(t *testing.T) {
	gw, upstream := testGatewayCfg(t, map[string]int{"key-a": 429, "key-b": 200}, true, func(c *Config) {
		c.Failover.MultiAccount = true
	})
	body := []byte(`{"model":"deepseek-v4-flash","messages":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ids := requestIDs{Session: "conv", Request: "r", Project: "p"}

	resp, err := gw.doUpstream(ctx, catalogTestRoute(), body, ids)
	if err != nil {
		t.Fatalf("doUpstream: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("client got HTTP %d, want 200 (conversation must continue on another account)", resp.StatusCode)
	}
	if seen := upstream.PerKeyCounts(); seen["key-a"] == 0 {
		t.Fatalf("expected key-a to be attempted before failing over, saw %v", seen)
	}
}

// TestQuotaParksAccountUntilWindow verifies a quota-exhausted account is
// parked out of rotation in multi-account mode and rejoins once the window
// elapses.
func TestQuotaParksAccountUntilWindow(t *testing.T) {
	cfg := PerformanceConfig{FailureCooldownSeconds: 1}
	transports, err := newTransportPool([]string{"direct"}, cfg, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := newNodePool([]string{"key-a", "key-b"}, transports, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMultiAccount(true)
	pool.MarkQuotaParked(pool.nodes[0], 40*time.Millisecond)

	if st := pool.StatusSnapshot()[0].State; st != "parked" {
		t.Fatalf("expected parked state, got %q", st)
	}
	if got := len(pool.ActiveOrder("")); got != 1 {
		t.Fatalf("parked account must be out of rotation, got %d active", got)
	}
	time.Sleep(60 * time.Millisecond)
	if st := pool.StatusSnapshot()[0].State; st != "active" {
		t.Fatalf("expected account to rejoin after park window, got state %q", st)
	}
}

// TestAllQuotaParkedReturnsExhausted ensures every-account-quota returns
// errQuotaExhausted (503) rather than looping forever.
func TestAllQuotaParkedReturnsExhausted(t *testing.T) {
	gw, _ := testGatewayCfg(t, map[string]int{"key-a": 429, "key-b": 402}, true, func(c *Config) {
		c.Failover.MultiAccount = true
		c.Failover.QuotaParkMinutes = 30
	})
	body := []byte(`{"model":"deepseek-v4-flash","messages":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := gw.doUpstream(ctx, catalogTestRoute(), body, requestIDs{Session: "s", Request: "r", Project: "p"})
	if !errors.Is(err, errQuotaExhausted) {
		t.Fatalf("expected errQuotaExhausted when every account is parked, got %v", err)
	}
}

// TestAllAccountsThrottledReturnsRetryable ensures that when every account is
// rate limited in multi-account mode the caller gets a retryable error, never
// a raw upstream 429 passthrough.
func TestAllAccountsThrottledReturnsRetryable(t *testing.T) {
	gw, _ := testGatewayCfg(t, map[string]int{}, true, func(c *Config) {
		c.Failover.MultiAccount = true
		// Throttle windows beyond the request deadline, so the loop must give
		// up with a retryable error instead of serving a raw 429 or spinning.
		c.Failover.Throttle = ThrottleConfig{InitialSeconds: 30, MaxSeconds: 600, Shared429Threshold: 2, MaxWaitSeconds: 2}
	})
	// Pre-throttle both accounts so every eligible node is limited before we
	// even attempt: the request loop must back off into a retryable error.
	gw.zenNodes.MarkNodeThrottled(gw.zenNodes.nodes[0], gw.cfg.Failover.Throttle)
	gw.zenNodes.MarkNodeThrottled(gw.zenNodes.nodes[1], gw.cfg.Failover.Throttle)

	body := []byte(`{"model":"deepseek-v4-flash","messages":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := gw.doUpstream(ctx, catalogTestRoute(), body, requestIDs{Session: "s", Request: "r", Project: "p"})
	if err == nil {
		// A raw upstream 429 would come back as a response here: unacceptable.
		t.Fatalf("expected retryable error when every account is throttled, got HTTP %d", resp.StatusCode)
	}
	if errors.Is(err, errAllCooling) {
		return
	}
	var te *throttleError
	if errors.As(err, &te) {
		return
	}
	t.Fatalf("expected errAllCooling or throttleError, got %v", err)
}
