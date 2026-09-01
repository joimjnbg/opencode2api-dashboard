package main

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// defaultLatencyBuckets are the standard Prometheus histogram boundaries
// for upstream request latency in seconds.
var defaultLatencyBuckets = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 25}

// histogramRecord is a lightweight, lock-free-by-convention histogram that
// mirrors Prometheus bucket semantics. Callers must hold the usageStats mutex.
type histogramRecord struct {
	Buckets  []float64
	Counts   []int64
	sum      float64
	totalObs int64
}

func newHistogram(buckets []float64) *histogramRecord {
	b := make([]float64, len(buckets))
	copy(b, buckets)
	return &histogramRecord{
		Buckets: b,
		Counts:  make([]int64, len(buckets)),
	}
}

// observe records a single observation, incrementing every bucket whose
// upper bound is ≥ the value, plus tracking the running sum.
func (h *histogramRecord) observe(value float64) {
	h.sum += value
	h.totalObs++
	for i, le := range h.Buckets {
		if value <= le {
			h.Counts[i]++
		}
	}
}

func (h *histogramRecord) TotalCount() int64 {
	return h.totalObs
}

func (h *histogramRecord) Sum() float64 {
	return h.sum
}

// BoundCount returns the count of observations ≤ le. If le matches a bucket
// exactly it returns that bucket's cumulative count. If le is past all buckets
// it returns total observations. If le is before all buckets it returns 0.
func (h *histogramRecord) BoundCount(le float64) int64 {
	for i, bound := range h.Buckets {
		if le <= bound {
			return h.Counts[i]
		}
	}
	return h.totalObs
}

// metricRecord aggregates counters for one model or one hour bucket.
type metricRecord struct {
	Requests        int     `json:"requests"`
	Success         int     `json:"success"`
	Failed          int     `json:"failed"`
	InputTokens     int     `json:"input_tokens"`
	OutputTokens    int     `json:"output_tokens"`
	CachedTokens    int     `json:"cached_tokens"`
	ReasoningTokens int     `json:"reasoning_tokens"`
	Cost            float64 `json:"cost"`
}

func (m *metricRecord) add(usage bridgeUsage, cost float64, ok bool) {
	m.Requests++
	if ok {
		m.Success++
	} else {
		m.Failed++
	}
	m.InputTokens += usage.Input
	m.OutputTokens += usage.Output
	m.CachedTokens += usage.Cached
	m.ReasoningTokens += usage.Reasoning
	m.Cost += cost
}

// usageStats keeps in-memory usage/cost metrics. It is intentionally small
// and resets nothing: this is a long-running gateway, so windows are derived
// from per-model records plus a sliding hourly histogram.
type usageStats struct {
	mu      sync.Mutex
	models  map[string]*metricRecord
	hours   map[string]*metricRecord
	started time.Time
	audit   *auditWriter

	// Prometheus-style histograms for upstream latency.
	upstreamLatency      map[string]*histogramRecord // keyed by tier ("zen", "go")
	upstreamLatencyModel map[string]*histogramRecord // keyed by "tier:model"

	// Retry counter per tier.
	retriesTotal map[string]*int64 // keyed by tier
}

func newUsageStats() *usageStats {
	return &usageStats{
		models:               map[string]*metricRecord{},
		hours:                map[string]*metricRecord{},
		started:              time.Now(),
		upstreamLatency:      map[string]*histogramRecord{},
		upstreamLatencyModel: map[string]*histogramRecord{},
		retriesTotal:         map[string]*int64{},
	}
}

// SetAudit configures a JSONL audit file. Returns the previous writer so the
// caller can decide whether to close it (only once at shutdown).
func (s *usageStats) SetAudit(a *auditWriter) {
	s.mu.Lock()
	s.audit = a
	s.mu.Unlock()
}

func (s *usageStats) Record(model string, usage bridgeUsage, cost float64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if model == "" {
		model = "unknown"
	}
	byModel, exists := s.models[model]
	if !exists {
		byModel = &metricRecord{}
		s.models[model] = byModel
	}
	byModel.add(usage, cost, ok)

	hourKey := time.Now().UTC().Truncate(time.Hour).Format(time.RFC3339)
	byHour, exists := s.hours[hourKey]
	if !exists {
		byHour = &metricRecord{}
		s.hours[hourKey] = byHour
	}
	byHour.add(usage, cost, ok)

	if s.audit != nil {
		s.audit.write(map[string]any{
			"ts":    time.Now().UTC().Format(time.RFC3339Nano),
			"model": model,
			"ok":    ok,
			"usage": usage,
			"cost":  cost,
		})
	}
}

// ObserveUpstreamLatency records an upstream request latency observation
// for the given tier.
func (s *usageStats) ObserveUpstreamLatency(tier string, latencySec float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.upstreamLatency[tier]
	if !ok {
		h = newHistogram(defaultLatencyBuckets)
		s.upstreamLatency[tier] = h
	}
	h.observe(latencySec)
}

// ObserveUpstreamLatencyModel records an upstream request latency observation
// for a specific tier:model combination.
func (s *usageStats) ObserveUpstreamLatencyModel(tier, model string, latencySec float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := tier + ":" + model
	h, ok := s.upstreamLatencyModel[key]
	if !ok {
		h = newHistogram(defaultLatencyBuckets)
		s.upstreamLatencyModel[key] = h
	}
	h.observe(latencySec)
}

// AddRetries increments the retry counter for the given tier by delta.
func (s *usageStats) AddRetries(tier string, delta int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.retriesTotal[tier]
	if !ok {
		v := int64(0)
		p = &v
		s.retriesTotal[tier] = p
	}
	*p += delta
}

func (s *usageStats) PruneOlderThan(window time.Duration) {
	cutoff := time.Now().UTC().Add(-window)
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.hours {
		t, err := time.Parse(time.RFC3339, key)
		if err == nil && t.Before(cutoff) {
			delete(s.hours, key)
		}
	}
}

// StartPrune runs a background goroutine that periodically drops hourly
// buckets older than the given window, preventing unbounded memory growth.
func (s *usageStats) StartPrune(ctx context.Context, window time.Duration, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.PruneOlderThan(window)
			}
		}
	}()
}

// Snapshot returns a JSON-serializable view of the stats.
func (s *usageStats) Snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	total := &metricRecord{}
	models := make([]map[string]any, 0, len(s.models))
	for name, rec := range s.models {
		models = append(models, map[string]any{"model": name, "stats": rec})
		total.Requests += rec.Requests
		total.Success += rec.Success
		total.Failed += rec.Failed
		total.InputTokens += rec.InputTokens
		total.OutputTokens += rec.OutputTokens
		total.CachedTokens += rec.CachedTokens
		total.ReasoningTokens += rec.ReasoningTokens
		total.Cost += rec.Cost
	}

	hours := make([]map[string]any, 0, len(s.hours))
	for key, rec := range s.hours {
		hours = append(hours, map[string]any{"hour": key, "stats": rec})
	}

	return map[string]any{
		"uptime_seconds":         int(time.Since(s.started).Seconds()),
		"total":                  total,
		"models":                 models,
		"hours":                  hours,
		"upstream_latency":       s.upstreamLatency,
		"upstream_latency_model": s.upstreamLatencyModel,
		"retries_total":          s.retriesTotal,
	}
}

func (s *usageStats) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Snapshot())
}
