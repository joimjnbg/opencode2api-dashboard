package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCatalogReplaceNilPreservesExisting(t *testing.T) {
	c := newModelCatalog(TierZen, nil, ModeOpenAI)
	c.Replace([]string{"gemini-3.7-flash", "gemini-2.5-pro"}, []string{"big-pickle", "hy3"})

	// Nil should preserve both tiers.
	c.Replace(nil, nil)
	snap := c.Snapshot()
	if snap.Zen != 2 {
		t.Errorf("zen preserved: got %d, want 2", snap.Zen)
	}
	if snap.Go != 2 {
		t.Errorf("go preserved: got %d, want 2", snap.Go)
	}
	if _, err := c.Route("gemini-3.7-flash", true, true); err != nil {
		t.Errorf("zen model still routable after nil replace: %v", err)
	}
	if _, err := c.Route("big-pickle", true, true); err != nil {
		t.Errorf("go model still routable after nil replace: %v", err)
	}
}

func TestCatalogReplaceNilZenUpdatesGo(t *testing.T) {
	c := newModelCatalog(TierZen, nil, ModeOpenAI)
	c.Replace([]string{"gemini-3.7-flash"}, []string{"big-pickle"})

	// Only zen updates; go preserved.
	c.Replace([]string{"gemini-3.7-flash", "gemini-2.5-flash"}, nil)
	snap := c.Snapshot()
	if snap.Zen != 2 {
		t.Errorf("zen updated: got %d, want 2", snap.Zen)
	}
	if snap.Go != 1 {
		t.Errorf("go preserved: got %d, want 1", snap.Go)
	}
}

func TestCatalogReplaceNilGoUpdatesZen(t *testing.T) {
	c := newModelCatalog(TierZen, nil, ModeOpenAI)
	c.Replace([]string{"gemini-3.7-flash"}, []string{"big-pickle"})

	// Only go updates; zen preserved.
	c.Replace(nil, []string{"big-pickle", "hy3"})
	snap := c.Snapshot()
	if snap.Zen != 1 {
		t.Errorf("zen preserved: got %d, want 1", snap.Zen)
	}
	if snap.Go != 2 {
		t.Errorf("go updated: got %d, want 2", snap.Go)
	}
}

func TestCatalogReplaceFullUpdate(t *testing.T) {
	c := newModelCatalog(TierZen, nil, ModeOpenAI)
	c.Replace([]string{"gemini-3.7-flash"}, []string{"big-pickle"})

	// Both tiers update.
	c.Replace([]string{"gemini-3.7-flash", "gemini-2.5-flash"}, []string{"big-pickle", "hy3"})
	snap := c.Snapshot()
	if snap.Zen != 2 || snap.Go != 2 {
		t.Errorf("both updated: zen=%d go=%d, want 2,2", snap.Zen, snap.Go)
	}
}

// --- Integration tests for auto-refresh with mock upstreams ---

// modelsHandler returns an HTTP handler that serves /v1/models with the given list.
func modelsHandler(models []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		data := ""
		for i, m := range models {
			if i > 0 {
				data += ","
			}
			data += `{"id":"` + m + `","object":"model"}`
		}
		_, _ = w.Write([]byte(`{"data":[` + data + `],"object":"list"}`))
	}
}

// modelsErrorHandler returns a handler that always returns 500.
func modelsErrorHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}
}

// modelsEmptyHandler returns a handler that returns an empty model list.
func modelsEmptyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[],"object":"list"}`))
	}
}

func testRefreshGateway(t *testing.T, zenURL, goURL string, static, staticGo []string) *Gateway {
	t.Helper()
	cfg := Config{
		Listen:       "127.0.0.1:0",
		ServerKeys:   []string{"local"},
		ZenKeys:      []string{"key-zen"},
		GoKeys:       []string{"key-go"},
		Proxies:      []string{"direct"},
		Upstream:     UpstreamConfig{Zen: zenURL, Go: goURL},
		UpstreamMode: ModeOpenAI,
		Retry:        RetryConfig{MaxAttempts: 1, TimeoutSeconds: 30},
		Models:       ModelsConfig{RefreshSeconds: 300, Static: static, StaticGo: staticGo, Protocols: map[string]string{}},
		Performance:  PerformanceConfig{FailureCooldownSeconds: 1},
		Logging:      LoggingConfig{Level: "error"},
		Stats:        StatsConfig{},
		Prefer:       TierZen,
		Sanitize:     SanitizeConfig{Enabled: true},
		Failover:     FailoverConfig{Enabled: true, Throttle: ThrottleConfig{InitialSeconds: 1, MaxSeconds: 60, MaxWaitSeconds: 5}},
		Fingerprint:  FingerprintConfig{Enabled: false},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw, err := NewGateway(cfg, logger)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	return gw
}

func TestRefreshTierSuccess(t *testing.T) {
	zen := httptest.NewServer(modelsHandler([]string{"gemini-3.7-flash", "gemini-2.5-pro"}))
	defer zen.Close()
	go_ := httptest.NewServer(modelsHandler([]string{"big-pickle", "hy3"}))
	defer go_.Close()

	gw := testRefreshGateway(t, zen.URL, go_.URL, nil, nil)
	ctx := context.Background()

	zenModels := gw.refreshTier(ctx, gw.cfg.Upstream.Zen, gw.zenNodes)
	goModels := gw.refreshTier(ctx, gw.cfg.Upstream.Go, gw.goNodes)

	if len(zenModels) != 2 {
		t.Errorf("zen models: got %d, want 2", len(zenModels))
	}
	if len(goModels) != 2 {
		t.Errorf("go models: got %d, want 2", len(goModels))
	}

	gw.catalog.Replace(zenModels, goModels)
	snap := gw.catalog.Snapshot()
	if snap.Zen != 2 || snap.Go != 2 {
		t.Errorf("catalog: zen=%d go=%d, want 2,2", snap.Zen, snap.Go)
	}
}

func TestRefreshTierUpstreamFailurePreservesCatalog(t *testing.T) {
	// Seed catalog with static models.
	gw := testRefreshGateway(t, "http://127.0.0.1:1", "http://127.0.0.1:1",
		[]string{"gemini-3.7-flash"}, []string{"big-pickle"})
	gw.catalog.Replace(gw.cfg.Models.Static, gw.cfg.Models.StaticGo)

	// Upstream is unreachable — refreshTier should return nil.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	zenModels := gw.refreshTier(ctx, gw.cfg.Upstream.Zen, gw.zenNodes)
	goModels := gw.refreshTier(ctx, gw.cfg.Upstream.Go, gw.goNodes)

	if zenModels != nil {
		t.Errorf("zen refresh should return nil on failure, got %v", zenModels)
	}
	if goModels != nil {
		t.Errorf("go refresh should return nil on failure, got %v", goModels)
	}

	// Catalog should still have the static seed.
	gw.catalog.Replace(zenModels, goModels) // nil-nil: preserves
	snap := gw.catalog.Snapshot()
	if snap.Zen != 1 {
		t.Errorf("zen catalog preserved: got %d, want 1", snap.Zen)
	}
	if snap.Go != 1 {
		t.Errorf("go catalog preserved: got %d, want 1", snap.Go)
	}
}

func TestRefreshTierServerErrorPreservesCatalog(t *testing.T) {
	errServer := httptest.NewServer(modelsErrorHandler())
	defer errServer.Close()

	gw := testRefreshGateway(t, errServer.URL, errServer.URL,
		[]string{"gemini-3.7-flash"}, []string{"big-pickle"})
	gw.catalog.Replace(gw.cfg.Models.Static, gw.cfg.Models.StaticGo)

	ctx := context.Background()
	zenModels := gw.refreshTier(ctx, gw.cfg.Upstream.Zen, gw.zenNodes)

	if zenModels != nil {
		t.Errorf("zen refresh should return nil on 500, got %v", zenModels)
	}

	// Catalog should still have the static seed.
	gw.catalog.Replace(nil, nil)
	snap := gw.catalog.Snapshot()
	if snap.Zen != 1 {
		t.Errorf("zen catalog preserved after 500: got %d, want 1", snap.Zen)
	}
}

func TestRefreshTierEmptyListPreservesCatalog(t *testing.T) {
	emptyServer := httptest.NewServer(modelsEmptyHandler())
	defer emptyServer.Close()

	gw := testRefreshGateway(t, emptyServer.URL, emptyServer.URL,
		[]string{"gemini-3.7-flash"}, []string{"big-pickle"})
	gw.catalog.Replace(gw.cfg.Models.Static, gw.cfg.Models.StaticGo)

	ctx := context.Background()
	zenModels := gw.refreshTier(ctx, gw.cfg.Upstream.Zen, gw.zenNodes)

	if zenModels != nil {
		t.Errorf("zen refresh should return nil on empty list, got %v", zenModels)
	}

	gw.catalog.Replace(nil, nil)
	snap := gw.catalog.Snapshot()
	if snap.Zen != 1 {
		t.Errorf("zen catalog preserved after empty list: got %d, want 1", snap.Zen)
	}
}

func TestRefreshTierPartialFailure(t *testing.T) {
	zen := httptest.NewServer(modelsHandler([]string{"gemini-3.7-flash", "gemini-2.5-pro"}))
	defer zen.Close()
	// Go tier is unreachable.
	gw := testRefreshGateway(t, zen.URL, "http://127.0.0.1:1",
		[]string{"gemini-3.7-flash"}, []string{"big-pickle"})
	gw.catalog.Replace(gw.cfg.Models.Static, gw.cfg.Models.StaticGo)

	ctx := context.Background()
	zenModels := gw.refreshTier(ctx, gw.cfg.Upstream.Zen, gw.zenNodes)
	goModels := gw.refreshTier(ctx, gw.cfg.Upstream.Go, gw.goNodes)

	// Zen should succeed, go should fail.
	gw.catalog.Replace(zenModels, goModels)
	snap := gw.catalog.Snapshot()
	if snap.Zen != 2 {
		t.Errorf("zen updated: got %d, want 2", snap.Zen)
	}
	if snap.Go != 1 {
		t.Errorf("go preserved from static: got %d, want 1", snap.Go)
	}
}
