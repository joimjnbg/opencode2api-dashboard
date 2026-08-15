package main

import (
	"encoding/json"
	"sync"
	"time"
)

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
}

func newUsageStats() *usageStats {
	return &usageStats{
		models:  map[string]*metricRecord{},
		hours:   map[string]*metricRecord{},
		started: time.Now(),
	}
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
		"uptime_seconds": int(time.Since(s.started).Seconds()),
		"total":          total,
		"models":         models,
		"hours":          hours,
	}
}

func (s *usageStats) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Snapshot())
}
