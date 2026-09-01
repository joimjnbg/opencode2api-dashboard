package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- unit tests for the shedder type ---

func TestShedderTryAcquire_WithinLimit(t *testing.T) {
	s := newShedder(2, 0)
	if !s.tryAcquire(TierZen) {
		t.Fatal("first acquire should succeed")
	}
	if !s.tryAcquire(TierZen) {
		t.Fatal("second acquire should succeed (within limit)")
	}
}

func TestShedderTryAcquire_Exhausted(t *testing.T) {
	s := newShedder(1, 0)
	if !s.tryAcquire(TierZen) {
		t.Fatal("first acquire should succeed")
	}
	if s.tryAcquire(TierZen) {
		t.Fatal("second acquire should fail (channel full)")
	}
}

func TestShedderRelease(t *testing.T) {
	s := newShedder(1, 0)
	if !s.tryAcquire(TierZen) {
		t.Fatal("acquire failed")
	}
	s.release(TierZen)
	if !s.tryAcquire(TierZen) {
		t.Fatal("acquire after release should succeed")
	}
}

func TestShedderSeparateTiers(t *testing.T) {
	s := newShedder(1, 1)
	if !s.tryAcquire(TierZen) {
		t.Fatal("zen acquire failed")
	}
	if !s.tryAcquire(TierGo) {
		t.Fatal("go acquire failed")
	}
	if s.tryAcquire(TierZen) {
		t.Fatal("zen should be exhausted")
	}
	if s.tryAcquire(TierGo) {
		t.Fatal("go should be exhausted")
	}
}

func TestShedderZeroLimit(t *testing.T) {
	s := newShedder(0, 0)
	for i := 0; i < 100; i++ {
		if !s.tryAcquire(TierZen) {
			t.Fatalf("acquire %d should succeed with unlimited mode", i)
		}
	}
}

// --- integration test: shedder wired into Gateway.doUpstream ---

func TestDoUpstreamShedderRejects(t *testing.T) {
	gw, _ := testGateway(t, map[string]int{"key-a": 200}, true)
	gw.shedder = newShedder(1, 0)
	if !gw.shedder.tryAcquire(TierZen) {
		t.Fatal("pre-acquire failed")
	}
	body := []byte(`{"model":"deepseek-v4-flash","messages":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := gw.doUpstream(ctx, catalogTestRoute(), body, requestIDs{Session: "s", Request: "r", Project: "p"})
	var se *shedError
	if !errors.As(err, &se) {
		t.Fatalf("expected *shedError, got %v", err)
	}
	if se.tier != TierZen {
		t.Errorf("shedError.tier = %q, want %q", se.tier, TierZen)
	}
	gw.shedder.release(TierZen)
}

func TestDoUpstreamShedderAcquiresAndReleases(t *testing.T) {
	gw, _ := testGateway(t, map[string]int{"key-a": 200}, true)
	gw.shedder = newShedder(1, 0)

	body := []byte(`{"model":"deepseek-v4-flash","messages":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := gw.doUpstream(ctx, catalogTestRoute(), body, requestIDs{Session: "s", Request: "r", Project: "p"})
	if err != nil {
		t.Fatalf("doUpstream: %v", err)
	}
	resp.Body.Close()
	// Semaphore must have been released (defer in doUpstream).
	resp2, err := gw.doUpstream(ctx, catalogTestRoute(), body, requestIDs{Session: "s", Request: "r2", Project: "p"})
	if err != nil {
		t.Fatalf("second doUpstream: %v", err)
	}
	resp2.Body.Close()
}

// --- integration test: shedder wired into handleInference returning 429 ---

func TestHandleInferenceShedderReturns429(t *testing.T) {
	gw, _ := testGateway(t, map[string]int{"key-a": 200}, true)
	gw.shedder = newShedder(1, 0)
	if !gw.shedder.tryAcquire(TierZen) {
		t.Fatal("pre-acquire failed")
	}
	defer gw.shedder.release(TierZen)

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer local")
	rec := httptest.NewRecorder()
	gw.handleInference(ProtocolChat)(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "1" {
		t.Errorf("Retry-After = %q, want %q", ra, "1")
	}
}

// --- stats counter test ---

func TestSheddedCounterInSnapshot(t *testing.T) {
	stats := newUsageStats()
	stats.AddShedded("zen", 3)
	stats.AddShedded("go", 1)
	snap := stats.Snapshot()
	shed, ok := snap["shedded_total"].(map[string]*int64)
	if !ok {
		t.Fatalf("shedded_total not in snapshot, got %T", snap["shedded_total"])
	}
	if got := *shed["zen"]; got != 3 {
		t.Errorf("shedded[zen] = %d, want 3", got)
	}
	if got := *shed["go"]; got != 1 {
		t.Errorf("shedded[go] = %d, want 1", got)
	}
}

// --- metrics output test ---

func TestMetricsShedded(t *testing.T) {
	gw, _ := testGateway(t, map[string]int{}, true)
	gw.stats.AddShedded("zen", 5)
	gw.stats.AddShedded("go", 2)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	gw.handleMetrics(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "opencode2api_requests_shedded_total") {
		t.Error("metrics output missing shedded_total")
	}
	if !strings.Contains(body, `{tier="zen"} 5`) {
		t.Error("metrics output missing zen shedded count")
	}
	if !strings.Contains(body, `{tier="go"} 2`) {
		t.Error("metrics output missing go shedded count")
	}
}

// --- priority test: streaming gets the slot first ---

func TestShedderPriorityStreamingFirst(t *testing.T) {
	s := newShedder(1, 0)
	streamPayload := map[string]any{"stream": true, "model": "m"}
	isStream := boolAt(streamPayload, "stream")
	if !isStream {
		t.Fatal("expected stream=true")
	}
	if !s.tryAcquire(TierZen) {
		t.Fatal("streaming acquire should succeed")
	}
	nonStreamPayload := map[string]any{"stream": false, "model": "m"}
	isStream2 := boolAt(nonStreamPayload, "stream")
	if isStream2 {
		t.Fatal("expected stream=false")
	}
	if s.tryAcquire(TierZen) {
		t.Fatal("non-streaming acquire should fail while streaming holds slot")
	}
	s.release(TierZen)
}

// --- Full HTTP round-trip through handler mux ---

func TestHTTPRoundTripShedder429(t *testing.T) {
	gw, _ := testGateway(t, map[string]int{"key-a": 200}, true)
	gw.shedder = newShedder(1, 0)
	if !gw.shedder.tryAcquire(TierZen) {
		t.Fatal("pre-acquire failed")
	}
	defer gw.shedder.release(TierZen)

	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer local")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", resp.StatusCode)
	}
	var errBody map[string]any
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&errBody)
	errMap, _ := errBody["error"].(map[string]any)
	if errMap["type"] != "rate_limit_exceeded" {
		t.Errorf("error type = %v, want rate_limit_exceeded", errMap["type"])
	}
}

func TestHTTPRoundTripShedderPasses(t *testing.T) {
	gw, _ := testGateway(t, map[string]int{"key-a": 200}, true)
	gw.shedder = newShedder(2, 0)

	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer local")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

// --- Async concurrency test ---

func TestShedderConcurrentAcquireRelease(t *testing.T) {
	s := newShedder(5, 0)
	const goroutines = 50
	const iterations = 100
	results := make(chan bool, goroutines*iterations)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				acquired := s.tryAcquire(TierZen)
				results <- acquired
				if acquired {
					time.Sleep(time.Microsecond)
					s.release(TierZen)
				}
			}
		}()
	}
	wg.Wait()
	close(results)
	total := 0
	succeeded := 0
	for r := range results {
		total++
		if r {
			succeeded++
		}
	}
	if succeeded == 0 {
		t.Error("no acquire succeeded across all goroutines")
	}
	if succeeded >= total {
		t.Error("all acquires succeeded; semaphore is not limiting")
	}
}

// --- NewGateway integration ---

func TestNewGatewayShedderFromConfig(t *testing.T) {
	upstream := newFakeUpstream(map[string]int{})
	server := httptest.NewServer(upstream.handler())
	t.Cleanup(server.Close)

	cfg := Config{
		Listen:      "127.0.0.1:0",
		ServerKeys:  []string{"local"},
		ZenKeys:     []string{"key-a"},
		GoKeys:      []string{},
		Proxies:     []string{"direct"},
		Upstream:    UpstreamConfig{Zen: server.URL, Go: server.URL},
		Retry:       RetryConfig{MaxAttempts: 1, TimeoutSeconds: 5},
		Models:      ModelsConfig{RefreshSeconds: 300, Protocols: map[string]string{}},
		Performance: PerformanceConfig{MaxIdleConns: 10, MaxIdleConnsPerHost: 10, MaxConnsPerHost: 0, IdleConnTimeoutSeconds: 30, ConnectTimeoutSeconds: 5, FailureCooldownSeconds: 1},
		Logging:     LoggingConfig{Level: "error"},
		Prefer:      TierGo,
		Sanitize:    SanitizeConfig{Enabled: false, ModelAliases: map[string]string{}},
		Failover: FailoverConfig{
			Enabled:              true,
			QuotaCooldownMinutes: 30,
			Throttle:             ThrottleConfig{InitialSeconds: 1, MaxSeconds: 10, Shared429Threshold: 2, MaxWaitSeconds: 5},
		},
		RateLimit:        RateLimitConfig{Enabled: false},
		MaxConcurrentZen: 3,
		MaxConcurrentGo:  2,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw, err := NewGateway(cfg, logger)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	if gw.shedder == nil {
		t.Fatal("shedder should be non-nil when MaxConcurrent > 0")
	}
	for i := 0; i < 3; i++ {
		if !gw.shedder.tryAcquire(TierZen) {
			t.Fatalf("zen acquire %d should succeed", i)
		}
	}
	if gw.shedder.tryAcquire(TierZen) {
		t.Fatal("zen acquire should fail at limit")
	}
	for i := 0; i < 2; i++ {
		if !gw.shedder.tryAcquire(TierGo) {
			t.Fatalf("go acquire %d should succeed", i)
		}
	}
	if gw.shedder.tryAcquire(TierGo) {
		t.Fatal("go acquire should fail at limit")
	}
}

func TestNewGatewayNoShedderByDefault(t *testing.T) {
	upstream := newFakeUpstream(map[string]int{})
	server := httptest.NewServer(upstream.handler())
	t.Cleanup(server.Close)

	cfg := Config{
		Listen:      "127.0.0.1:0",
		ServerKeys:  []string{"local"},
		ZenKeys:     []string{"key-a"},
		GoKeys:      []string{},
		Proxies:     []string{"direct"},
		Upstream:    UpstreamConfig{Zen: server.URL, Go: server.URL},
		Retry:       RetryConfig{MaxAttempts: 1, TimeoutSeconds: 5},
		Models:      ModelsConfig{RefreshSeconds: 300, Protocols: map[string]string{}},
		Performance: PerformanceConfig{MaxIdleConns: 10, MaxIdleConnsPerHost: 10, MaxConnsPerHost: 0, IdleConnTimeoutSeconds: 30, ConnectTimeoutSeconds: 5, FailureCooldownSeconds: 1},
		Logging:     LoggingConfig{Level: "error"},
		Prefer:      TierGo,
		Sanitize:    SanitizeConfig{Enabled: false, ModelAliases: map[string]string{}},
		Failover: FailoverConfig{
			Enabled:              true,
			QuotaCooldownMinutes: 30,
			Throttle:             ThrottleConfig{InitialSeconds: 1, MaxSeconds: 10, Shared429Threshold: 2, MaxWaitSeconds: 5},
		},
		RateLimit: RateLimitConfig{Enabled: false},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw, err := NewGateway(cfg, logger)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	if gw.shedder != nil {
		t.Fatal("shedder should be nil when MaxConcurrent = 0")
	}
}

// Validate config rejects negative concurrency limits.
func TestConfigRejectsNegativeConcurrency(t *testing.T) {
	goodJSON := `{"server_keys":["k"],"zen_keys":["k"],"max_concurrent_zen":-1}`
	tmp := t.TempDir() + "/cfg.json"
	if err := os.WriteFile(tmp, []byte(goodJSON), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(tmp)
	if err == nil {
		t.Fatal("expected error for negative max_concurrent_zen")
	}
	if !strings.Contains(err.Error(), "max_concurrent") {
		t.Errorf("error = %q, want it to mention max_concurrent", err.Error())
	}
}
