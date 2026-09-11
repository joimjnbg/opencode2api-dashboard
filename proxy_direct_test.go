package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

// timeoutRoundTripper fails every request with a timeout-shaped error, so
// checkClaimedProxy classifies it as a proxy failure (isProxyFailure).
type timeoutRoundTripper struct{}

func (timeoutRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, context.DeadlineExceeded
}

type genericRoundTripper struct{ err error }

func (g genericRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, g.err
}

func replaceClient(t *testing.T, p *proxyTransport, rt http.RoundTripper) {
	t.Helper()
	p.client = &http.Client{Transport: rt}
}

// Seam A: checkClaimedProxy (pool.go) — direct egress must never be marked
// unhealthy, even when the probe itself times out.
func TestDirectProbeTimeoutKeepsHealthy(t *testing.T) {
	cfg := PerformanceConfig{FailureCooldownSeconds: 1}
	transports, err := newTransportPool([]string{"direct"}, cfg, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	direct := transports.items[0]
	replaceClient(t, direct, timeoutRoundTripper{})
	if !direct.checking.CompareAndSwap(false, true) {
		t.Fatal("failed to claim direct proxy for check")
	}

	result := transports.checkClaimedProxy(context.Background(), direct, "http://probe.invalid/", time.Second)

	if result.failed {
		t.Error("direct probe timeout must not be classified as failed")
	}
	if !direct.healthy.Load() {
		t.Error("direct must stay healthy during an upstream timeout probe (fallback tier depends on it)")
	}
	if !result.wasHealthy {
		t.Error("wasHealthy must stay true for direct so applyProxyHealthResult never rebinds")
	}
}

// Seam A contrast: a real proxy that times out IS marked failed/unhealthy.
func TestRealProxyTimeoutMarksFailed(t *testing.T) {
	cfg := PerformanceConfig{FailureCooldownSeconds: 1}
	transports, err := newTransportPool([]string{"http://127.0.0.1:1"}, cfg, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	proxy := transports.items[0]
	replaceClient(t, proxy, timeoutRoundTripper{})
	if !proxy.checking.CompareAndSwap(false, true) {
		t.Fatal("failed to claim proxy for check")
	}

	result := transports.checkClaimedProxy(context.Background(), proxy, "http://probe.invalid/", time.Second)

	if !result.failed {
		t.Error("real proxy timeout must be classified as failed")
	}
	if proxy.healthy.Load() {
		t.Error("real proxy must be marked unhealthy after timeout")
	}
}

// Seam A: inconclusive (non-proxy) errors must leave health untouched for both.
func TestProbeInconclusiveErrorKeepsHealth(t *testing.T) {
	cfg := PerformanceConfig{FailureCooldownSeconds: 1}
	transports, err := newTransportPool([]string{"direct"}, cfg, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	direct := transports.items[0]
	replaceClient(t, direct, genericRoundTripper{err: errors.New("boom")})
	if !direct.checking.CompareAndSwap(false, true) {
		t.Fatal("failed to claim direct proxy for check")
	}

	result := transports.checkClaimedProxy(context.Background(), direct, "http://probe.invalid/", time.Second)

	if result.failed {
		t.Error("non-proxy error must not be classified as failed")
	}
	if !direct.healthy.Load() {
		t.Error("inconclusive probe must not touch direct health")
	}
}

// Seam B: applyProxyHealthResult (gateway.go) — a failed direct result must
// restore health and never rebind keys off direct.
func TestApplyDirectFailureRestoresWithoutRebind(t *testing.T) {
	gw, _ := testGateway(t, map[string]int{"key-a": 200}, false)
	direct := gw.transports.items[0]
	direct.healthy.Store(false)

	result := proxyHealthResult{proxy: direct, err: context.DeadlineExceeded, failed: true, wasHealthy: false}
	gw.applyProxyHealthResult(result, "test", 504)

	if !direct.healthy.Load() {
		t.Fatal("direct must be restored to healthy even on probe failure")
	}
	for _, node := range gw.zenNodes.nodes {
		if got := int(node.proxyIndex.Load()); got != 0 {
			t.Errorf("no key may be rebound off direct, node %d on proxy %d", node.index, got)
		}
	}
	if !gw.zenNodes.nodeEligible(gw.zenNodes.nodes[0], time.Now().UnixNano()) {
		t.Error("direct-bound key must stay eligible after failed probe")
	}
}

// Seam B contrast: failed real proxy still rebinds keys to a healthy one.
func TestApplyRealProxyFailureRebinds(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := PerformanceConfig{FailureCooldownSeconds: 1}
	transports, err := newTransportPool([]string{"http://127.0.0.1:1", "http://127.0.0.1:2"}, cfg, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := newNodePool([]string{"key-a"}, transports, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	gw := &Gateway{logger: logger, transports: transports, zenNodes: pool, goNodes: pool}
	bad := transports.items[0]
	bad.healthy.Store(false)

	result := proxyHealthResult{proxy: bad, err: context.DeadlineExceeded, failed: true, wasHealthy: true}
	gw.applyProxyHealthResult(result, "test", 504)

	if got := int(pool.nodes[0].proxyIndex.Load()); got != 1 {
		t.Errorf("key must be rebound to proxy 1, got %d", got)
	}
}
