package main

import (
	"bytes"
	"context"
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
		Failover:    FailoverConfig{Enabled: true, QuotaCooldownMinutes: 30, TreatGeneric429AsQuota: false},
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
