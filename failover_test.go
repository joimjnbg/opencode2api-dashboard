package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeUpstream counts which keys were seen and answers each request.
type fakeUpstream struct {
	// keyAnswers maps upstream key -> HTTP status to return. A nil value
	// returns 200 with a canned chat completion.
	keyAnswers map[string]int
	seenKeys   atomic.Int32
	machineIDs map[string]string
	// quotaFirst makes the first observed request return a quota error and
	// everything afterwards succeed, regardless of which key is selected.
	quotaFirst atomic.Int32
	// rateLimitFirst makes the first N observed requests return a generic 429
	// ("Rate limit exceeded", NOT a quota body) and everything afterwards
	// succeed. Used to exercise the account-throttle path.
	rateLimitFirst atomic.Int32
}

func newFakeUpstream(answers map[string]int) *fakeUpstream {
	f := &fakeUpstream{keyAnswers: answers, machineIDs: map[string]string{}}
	return f
}

func (f *fakeUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		key := strings.TrimPrefix(auth, "Bearer ")
		f.machineIDs[key] = r.Header.Get("x-machine-id")

		status, known := f.keyAnswers[key]
		if !known || status == 0 {
			status = 200
		}
		if f.quotaFirst.CompareAndSwap(1, 0) {
			// Only the first observed request in the test returns quota error.
			f.seenKeys.Add(1)
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":{"message":"Free usage exceeded, subscribe to Go","type":"rate_limit_exceeded"}}`))
			return
		}
		for {
			left := f.rateLimitFirst.Load()
			if left <= 0 {
				break
			}
			if f.rateLimitFirst.CompareAndSwap(left, left-1) {
				f.seenKeys.Add(1)
				w.WriteHeader(429)
				_, _ = w.Write([]byte(`{"error":{"message":"Rate limit exceeded. Please try again later.","type":"FreeUsageLimitError"}}`))
				return
			}
		}
		f.seenKeys.Add(1)
		w.WriteHeader(status)
		switch status {
		case 400:
			_, _ = w.Write([]byte(`{"error":{"message":"bad request","type":"invalid_request_error"}}`))
			return
		case 429, 402:
			_, _ = w.Write([]byte(`{"error":{"message":"Free usage exceeded, subscribe to Go","type":"rate_limit_exceeded"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"cmpl-test","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	})
}

// testGateway builds a Gateway wired to a fake upstream with the given zen keys.
func testGateway(t *testing.T, keyAnswers map[string]int, fingerprintEnabled bool) (*Gateway, *fakeUpstream) {
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw, err := NewGateway(cfg, logger)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	return gw, upstream
}

func catalogTestRoute() modelRoute {
	// model "deepseek-v4-flash" is a chat model on zen tier.
	return modelRoute{ID: "deepseek-v4-flash", Tier: TierZen, Protocol: ProtocolChat}
}

func TestSilentFailoverRetriesOnNextKey(t *testing.T) {
	// The first upstream response is a 429 quota error; the gateway must
	// silently retry with the remaining key and return 200 to the caller.
	gw, upstream := testGateway(t, map[string]int{"key-a": 200, "key-b": 200}, true)
	upstream.quotaFirst.Store(1)

	body := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	ids := requestIDs{Session: "ses-test", Request: "req-test", Project: "prj-test"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := gw.doUpstream(ctx, catalogTestRoute(), body, ids)
	if err != nil {
		t.Fatalf("doUpstream returned error (client would see 5xx): %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("client received HTTP %d, want 200 (silent failover failed)", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(raw, []byte(`"content":"ok"`)) {
		t.Errorf("response body unexpected: %s", raw)
	}
	// The exhausted key must be cooling down now: exactly one of the two.
	if got := gw.zenNodes.Cooling(); got != 1 {
		t.Errorf("expected 1 cooling node, got %d", got)
	}
	// Both keys were attempted once (quota first, then success).
	if seen := upstream.seenKeys.Load(); seen != 2 {
		t.Errorf("expected 2 upstream attempts for silent failover, got %d", seen)
	}
}

func TestSilentFailoverAllKeysQuota(t *testing.T) {
	gw, _ := testGateway(t, map[string]int{"key-a": 429, "key-b": 402}, true)
	body := []byte(`{"model":"deepseek-v4-flash","messages":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := gw.doUpstream(ctx, catalogTestRoute(), body, requestIDs{Session: "s", Request: "r", Project: "p"})
	if err == nil {
		t.Fatal("expected errQuotaExhausted when every key is out of quota")
	}
	// Both keys cooling.
	if got := gw.zenNodes.Cooling(); got != 2 {
		t.Errorf("expected 2 cooling nodes, got %d", got)
	}
}

func TestRequestShapeErrorNotRetried(t *testing.T) {
	// A 400 (bad request) must be returned to the caller untouched, not rotated.
	gw, _ := testGateway(t, map[string]int{"key-a": 400, "key-b": 400}, true)
	body := []byte(`{"model":"deepseek-v4-flash","messages":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := gw.doUpstream(ctx, catalogTestRoute(), body, requestIDs{Session: "s", Request: "r", Project: "p"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Errorf("request-shape error: got %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	if got := gw.zenNodes.Cooling(); got != 0 {
		t.Errorf("request-shape errors must not cool keys, got %d cooling", got)
	}
}

func TestFingerprintHeadersAttachedPerKey(t *testing.T) {
	gw, upstream := testGateway(t, map[string]int{}, true)
	upstream.quotaFirst.Store(1) // force both keys to be visited
	body := []byte(`{"model":"deepseek-v4-flash","messages":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := gw.doUpstream(ctx, catalogTestRoute(), body, requestIDs{Session: "s", Request: "r", Project: "p"})
	if err != nil {
		t.Fatalf("doUpstream: %v", err)
	}
	resp.Body.Close()
	if upstream.machineIDs["key-a"] == "" || upstream.machineIDs["key-b"] == "" {
		t.Errorf("missing machine-id fingerprints: %v", upstream.machineIDs)
	}
	if upstream.machineIDs["key-a"] == upstream.machineIDs["key-b"] {
		t.Error("different keys share the same machine id")
	}
	if len(upstream.machineIDs["key-a"]) != 36 {
		t.Errorf("machine id not a UUID: %q", upstream.machineIDs["key-a"])
	}
}

func TestQuotaHasNoMachineIDWhenDisabled(t *testing.T) {
	gw, upstream := testGateway(t, map[string]int{"key-a": 400, "key-b": 400}, false)
	body := []byte(`{"model":"deepseek-v4-flash","messages":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := gw.doUpstream(ctx, catalogTestRoute(), body, requestIDs{Session: "s", Request: "r", Project: "p"})
	if err != nil {
		t.Fatalf("doUpstream: %v", err)
	}
	resp.Body.Close()
	for key, id := range upstream.machineIDs {
		if id != "" {
			t.Errorf("fingerprint injected while disabled for %s: %q", key, id)
		}
	}
}

// TestShared429BackpressureThrottlesThenRecovers exercises the account-level
// circuit breaker: both keys hit a generic 429 (not a quota body), the pool
// detects the shared limit, holds the request until the throttle window
// elapses, then a probe succeeds and clears the throttle.
func TestShared429BackpressureThrottlesThenRecovers(t *testing.T) {
	// Keys answer 200 by default; the first two observed requests return a
	// generic 429, then everything succeeds.
	gw, upstream := testGateway(t, map[string]int{}, true)
	upstream.rateLimitFirst.Store(2)

	body := []byte(`{"model":"deepseek-v4-flash","messages":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	resp, err := gw.doUpstream(ctx, catalogTestRoute(), body, requestIDs{Session: "s", Request: "r", Project: "p"})
	if err != nil {
		t.Fatalf("doUpstream: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 after backpressure recovery, got %d", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("request returned too fast (%v): the throttle window was not waited out", elapsed)
	}
	// The probe succeeded, so the account throttle must be cleared.
	if !gw.zenNodes.ThrottleDeadline().IsZero() {
		t.Error("account throttle still active after a successful probe")
	}
	if seen := upstream.seenKeys.Load(); seen != 3 {
		t.Errorf("expected 3 upstream attempts (2 rejected + 1 probe), got %d", seen)
	}
}

// TestShared429BeyondWaitReturnsThrottleError ensures that when the throttle
// window outlasts the backpressure budget, the gateway gives up with a
// throttleError (503 + Retry-After) instead of failing keys forever.
func TestShared429BeyondWaitReturnsThrottleError(t *testing.T) {
	// Both keys keep returning generic 429s for the whole test: the throttle
	// window (30s) exceeds the 5s max wait, so the gateway must give up with
	// a throttleError (503 + Retry-After) instead of looping forever.
	gw, upstream := testGateway(t, map[string]int{}, true)
	upstream.rateLimitFirst.Store(1 << 30)
	body := []byte(`{"model":"deepseek-v4-flash","messages":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	gw.cfg.Failover.Throttle.InitialSeconds = 30 // window > max wait budget
	_, err := gw.doUpstream(ctx, catalogTestRoute(), body, requestIDs{Session: "s", Request: "r", Project: "p"})
	var te *throttleError
	if !errors.As(err, &te) {
		t.Fatalf("expected *throttleError, got %v", err)
	}
	if te.retryAfter <= 0 {
		t.Errorf("throttleError must carry a positive retryAfter, got %v", te.retryAfter)
	}
	if gw.zenNodes.ThrottleDeadline().IsZero() {
		t.Error("account throttle must remain active")
	}
}

// TestRecord429Threshold verifies the shared-limit detector directly.
func TestRecord429Threshold(t *testing.T) {
	cfg := PerformanceConfig{FailureCooldownSeconds: 1}
	transports, err := newTransportPool([]string{"direct"}, cfg, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := newNodePool([]string{"key-a", "key-b", "key-c"}, transports, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if pool.Record429(pool.nodes[0], time.Minute, 2) {
		t.Error("one key 429 must not trip the shared-limit detector")
	}
	if pool.Record429(pool.nodes[1], time.Minute, 3) {
		t.Error("two keys but threshold 3 must not trip")
	}
	if !pool.Record429(pool.nodes[2], time.Minute, 2) {
		t.Error("three distinct keys 429 inside the window must trip")
	}
}

// TestMarkAccountThrottledExponential verifies the window grows on repeated
// 429s and resets after a success.
func TestMarkAccountThrottledExponential(t *testing.T) {
	cfg := PerformanceConfig{FailureCooldownSeconds: 1}
	transports, err := newTransportPool([]string{"direct"}, cfg, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := newNodePool([]string{"key-a"}, transports, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pool.MarkAccountThrottled(ThrottleConfig{InitialSeconds: 1, MaxSeconds: 600, Shared429Threshold: 2, MaxWaitSeconds: 5})
	first := pool.ThrottleDeadline()
	if first.IsZero() {
		t.Fatal("throttle not active after MarkAccountThrottled")
	}
	pool.MarkAccountThrottled(ThrottleConfig{InitialSeconds: 1, MaxSeconds: 600, Shared429Threshold: 2, MaxWaitSeconds: 5})
	second := pool.ThrottleDeadline()
	if second.Sub(first) < 900*time.Millisecond {
		t.Errorf("expected the window to double, got %v", second.Sub(first))
	}
	pool.ClearAccountThrottle()
	if !pool.ThrottleDeadline().IsZero() {
		t.Error("throttle must clear after a successful probe")
	}
}
