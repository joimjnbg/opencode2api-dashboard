package main

import (
	"testing"
)

func TestHistogramRecord_Observe(t *testing.T) {
	buckets := []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 25}
	h := newHistogram(buckets)

	// Observe values: 0.05, 0.15, 0.4, 0.8, 3.0, 7.0, 30.0
	h.observe(0.05) // falls into bucket 0.1
	h.observe(0.15) // falls into bucket 0.25
	h.observe(0.4)  // falls into bucket 0.5
	h.observe(0.8)  // falls into bucket 1
	h.observe(3.0)  // falls into bucket 5
	h.observe(7.0)  // falls into bucket 10
	h.observe(30.0) // falls into +Inf (past 25)

	// Cumulative bucket counts
	// 0.1: 1, 0.25: 2, 0.5: 3, 1: 4, 2.5: 4, 5: 5, 10: 6, 25: 6
	// +Inf: 7
	expectedCounts := []int64{1, 2, 3, 4, 4, 5, 6, 6}
	for i, want := range expectedCounts {
		if got := h.Counts[i]; got != want {
			t.Errorf("bucket %v: got count %d, want %d", buckets[i], got, want)
		}
	}
	if got := h.TotalCount(); got != 7 {
		t.Errorf("TotalCount() = %d, want 7", got)
	}

	// Sum of observed values: 0.05+0.15+0.4+0.8+3+7+30 = 41.4
	wantSum := 41.4
	if got := h.Sum(); got < wantSum-0.001 || got > wantSum+0.001 {
		t.Errorf("Sum() = %f, want %f", got, wantSum)
	}
}

func TestUsageStats_ObserveUpstreamLatency(t *testing.T) {
	stats := newUsageStats()

	stats.ObserveUpstreamLatency("zen", 0.5)
	stats.ObserveUpstreamLatency("zen", 1.5)
	stats.ObserveUpstreamLatency("go", 0.3)

	stats.mu.Lock()
	defer stats.mu.Unlock()

	zenHist := stats.upstreamLatency["zen"]
	if zenHist == nil {
		t.Fatal("upstreamLatency[zen] is nil")
	}
	if got := zenHist.TotalCount(); got != 2 {
		t.Errorf("zen latency count = %d, want 2", got)
	}

	goHist := stats.upstreamLatency["go"]
	if goHist == nil {
		t.Fatal("upstreamLatency[go] is nil")
	}
	if got := goHist.TotalCount(); got != 1 {
		t.Errorf("go latency count = %d, want 1", got)
	}
}

func TestUsageStats_ObserveUpstreamLatencyModel(t *testing.T) {
	stats := newUsageStats()

	stats.ObserveUpstreamLatencyModel("zen", "claude-sonnet-4-5-free", 0.3)
	stats.ObserveUpstreamLatencyModel("zen", "claude-sonnet-4-5-free", 0.8)

	stats.mu.Lock()
	defer stats.mu.Unlock()

	key := "zen:claude-sonnet-4-5-free"
	hist := stats.upstreamLatencyModel[key]
	if hist == nil {
		t.Fatalf("upstreamLatencyModel[%q] is nil", key)
	}
	if got := hist.TotalCount(); got != 2 {
		t.Errorf("model latency count = %d, want 2", got)
	}
}

func TestUsageStats_AddRetries(t *testing.T) {
	stats := newUsageStats()

	stats.AddRetries("zen", 1)
	stats.AddRetries("zen", 2)
	stats.AddRetries("go", 1)

	stats.mu.Lock()
	defer stats.mu.Unlock()

	if got := *stats.retriesTotal["zen"]; got != 3 {
		t.Errorf("retries[zen] = %d, want 3", got)
	}
	if got := *stats.retriesTotal["go"]; got != 1 {
		t.Errorf("retries[go] = %d, want 1", got)
	}
}
