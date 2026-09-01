package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// writeMetrics renders the in-memory usage stats as Prometheus text format
// (expfmt). It depends only on the standard library, keeping the gateway
// dependency-free.
func (g *Gateway) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	snapshot := g.stats.Snapshot()

	total, _ := snapshot["total"].(*metricRecord)
	uptime, _ := snapshot["uptime_seconds"].(int)
	models, _ := snapshot["models"].([]map[string]any)
	if total == nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var out strings.Builder

	writeCounter := func(name, help string, value int) {
		out.WriteString("# HELP " + name + " " + help + "\n")
		out.WriteString("# TYPE " + name + " counter\n")
		fmt.Fprintf(&out, "%s %d\n", name, value)
	}

	out.WriteString("# HELP opencode2api_up 1 if the gateway is serving traffic\n")
	out.WriteString("# TYPE opencode2api_up gauge\n")
	out.WriteString("opencode2api_up 1\n")

	fmt.Fprintf(&out, "# HELP opencode2api_uptime_seconds Gateway uptime in seconds\n")
	out.WriteString("# TYPE opencode2api_uptime_seconds gauge\n")
	fmt.Fprintf(&out, "opencode2api_uptime_seconds %d\n", int64(uptime))

	writeCounter("opencode2api_requests_total", "Total requests received", total.Requests)
	writeCounter("opencode2api_requests_success_total", "Successful upstream requests", total.Success)
	writeCounter("opencode2api_requests_failed_total", "Failed upstream requests", total.Failed)
	writeCounter("opencode2api_tokens_input_total", "Input tokens (includes cached)", total.InputTokens)
	writeCounter("opencode2api_tokens_output_total", "Output tokens", total.OutputTokens)
	writeCounter("opencode2api_tokens_cached_total", "Cached input tokens", total.CachedTokens)
	writeCounter("opencode2api_tokens_reasoning_total", "Reasoning tokens", total.ReasoningTokens)

	out.WriteString("# HELP opencode2api_cost_total Total monetary cost in USD\n")
	out.WriteString("# TYPE opencode2api_cost_total counter\n")
	fmt.Fprintf(&out, "opencode2api_cost_total %.6f\n", total.Cost)

	// Per-model metrics.
	sort.Slice(models, func(i, j int) bool {
		nameI, _ := models[i]["model"].(string)
		nameJ, _ := models[j]["model"].(string)
		return nameI < nameJ
	})
	if len(models) > 0 {
		out.WriteString("# HELP opencode2api_model_requests_total Requests per model\n")
		out.WriteString("# TYPE opencode2api_model_requests_total counter\n")
		out.WriteString("# HELP opencode2api_model_tokens_input_total Input tokens per model\n")
		out.WriteString("# TYPE opencode2api_model_tokens_input_total counter\n")
		out.WriteString("# HELP opencode2api_model_tokens_output_total Output tokens per model\n")
		out.WriteString("# TYPE opencode2api_model_tokens_output_total counter\n")
		out.WriteString("# HELP opencode2api_model_cost_total Cost per model in USD\n")
		out.WriteString("# TYPE opencode2api_model_cost_total counter\n")
		for _, item := range models {
			modelName, _ := item["model"].(string)
			rec, _ := item["stats"].(*metricRecord)
			if rec == nil {
				continue
			}
			model := escapeLabel(modelName)
			fmt.Fprintf(&out, "opencode2api_model_requests_total{model=%q} %d\n", model, rec.Requests)
			fmt.Fprintf(&out, "opencode2api_model_tokens_input_total{model=%q} %d\n", model, rec.InputTokens)
			fmt.Fprintf(&out, "opencode2api_model_tokens_output_total{model=%q} %d\n", model, rec.OutputTokens)
			fmt.Fprintf(&out, "opencode2api_model_cost_total{model=%q} %.6f\n", model, rec.Cost)
		}
	}

	// Upstream latency histograms by tier.
	latencyHists, _ := snapshot["upstream_latency"].(map[string]*histogramRecord)
	if len(latencyHists) > 0 {
		out.WriteString("# HELP opencode2api_upstream_latency_seconds Upstream request latency\n")
		out.WriteString("# TYPE opencode2api_upstream_latency_seconds histogram\n")
		// Sort tier keys for deterministic output.
		tierKeys := make([]string, 0, len(latencyHists))
		for k := range latencyHists {
			tierKeys = append(tierKeys, k)
		}
		sort.Strings(tierKeys)
		for _, tier := range tierKeys {
			h := latencyHists[tier]
			for i, le := range h.Buckets {
				fmt.Fprintf(&out, "opencode2api_upstream_latency_seconds_bucket{tier=%q,le=%q} %d\n", tier, fmt.Sprintf("%g", le), h.Counts[i])
			}
			fmt.Fprintf(&out, "opencode2api_upstream_latency_seconds_bucket{tier=%q,le=%q} %d\n", tier, "+Inf", h.TotalCount())
			fmt.Fprintf(&out, "opencode2api_upstream_latency_seconds_sum{tier=%q} %.6f\n", tier, h.Sum())
			fmt.Fprintf(&out, "opencode2api_upstream_latency_seconds_count{tier=%q} %d\n", tier, h.TotalCount())
		}
	}

	// Upstream latency histograms per tier:model.
	latencyModelHists, _ := snapshot["upstream_latency_model"].(map[string]*histogramRecord)
	if len(latencyModelHists) > 0 {
		out.WriteString("# HELP opencode2api_upstream_latency_model_seconds Upstream request latency per tier:model\n")
		out.WriteString("# TYPE opencode2api_upstream_latency_model_seconds histogram\n")
		modelKeys := make([]string, 0, len(latencyModelHists))
		for k := range latencyModelHists {
			modelKeys = append(modelKeys, k)
		}
		sort.Strings(modelKeys)
		for _, key := range modelKeys {
			h := latencyModelHists[key]
			// key is "tier:model"
			parts := strings.SplitN(key, ":", 2)
			tier, model := parts[0], parts[1]
			for i, le := range h.Buckets {
				fmt.Fprintf(&out, "opencode2api_upstream_latency_model_seconds_bucket{tier=%q,model=%q,le=%q} %d\n", tier, model, fmt.Sprintf("%g", le), h.Counts[i])
			}
			fmt.Fprintf(&out, "opencode2api_upstream_latency_model_seconds_bucket{tier=%q,model=%q,le=%q} %d\n", tier, model, "+Inf", h.TotalCount())
			fmt.Fprintf(&out, "opencode2api_upstream_latency_model_seconds_sum{tier=%q,model=%q} %.6f\n", tier, model, h.Sum())
			fmt.Fprintf(&out, "opencode2api_upstream_latency_model_seconds_count{tier=%q,model=%q} %d\n", tier, model, h.TotalCount())
		}
	}

	// Retry counts by tier.
	retries, _ := snapshot["retries_total"].(map[string]*int64)
	if len(retries) > 0 {
		out.WriteString("# HELP opencode2api_retries_total Total upstream retry attempts\n")
		out.WriteString("# TYPE opencode2api_retries_total counter\n")
		retryKeys := make([]string, 0, len(retries))
		for k := range retries {
			retryKeys = append(retryKeys, k)
		}
		sort.Strings(retryKeys)
		for _, tier := range retryKeys {
			fmt.Fprintf(&out, "opencode2api_retries_total{tier=%q} %d\n", tier, *retries[tier])
		}
	}

	// Shedded request counts by tier.
	shedded, _ := snapshot["shedded_total"].(map[string]*int64)
	if len(shedded) > 0 {
		out.WriteString("# HELP opencode2api_requests_shedded_total Requests rejected by concurrency limiter\n")
		out.WriteString("# TYPE opencode2api_requests_shedded_total counter\n")
		shedKeys := make([]string, 0, len(shedded))
		for k := range shedded {
			shedKeys = append(shedKeys, k)
		}
		sort.Strings(shedKeys)
		for _, tier := range shedKeys {
			fmt.Fprintf(&out, "opencode2api_requests_shedded_total{tier=%q} %d\n", tier, *shedded[tier])
		}
	}

	// Health facts.
	proxyTotal, proxyHealthy := g.transports.healthCounts()
	fmt.Fprintf(&out, "# HELP opencode2api_proxies_total Configured proxy transports\n")
	out.WriteString("# TYPE opencode2api_proxies_total gauge\n")
	fmt.Fprintf(&out, "opencode2api_proxies_total %d\n", proxyTotal)
	fmt.Fprintf(&out, "# HELP opencode2api_proxies_healthy Healthy proxy transports\n")
	out.WriteString("# TYPE opencode2api_proxies_healthy gauge\n")
	fmt.Fprintf(&out, "opencode2api_proxies_healthy %d\n", proxyHealthy)

	keys := snapshotKeys(g)
	fmt.Fprintf(&out, "# HELP opencode2api_keys_total Configured upstream keys\n")
	out.WriteString("# TYPE opencode2api_keys_total gauge\n")
	fmt.Fprintf(&out, "opencode2api_keys_total %d\n", keys)

	zenCooling, goCooling := g.zenNodes.Cooling(), g.goNodes.Cooling()
	zenThrottled, goThrottled := g.zenNodes.ThrottledKeyCount(), g.goNodes.ThrottledKeyCount()
	zenParked, goParked := g.zenNodes.Parked(), g.goNodes.Parked()
	fmt.Fprintf(&out, "# HELP opencode2api_keys_cooling_total Upstream keys currently cooling down\n")
	out.WriteString("# TYPE opencode2api_keys_cooling_total gauge\n")
	fmt.Fprintf(&out, "opencode2api_keys_cooling_total{tier=\"zen\"} %d\n", zenCooling)
	fmt.Fprintf(&out, "opencode2api_keys_cooling_total{tier=\"go\"} %d\n", goCooling)
	fmt.Fprintf(&out, "# HELP opencode2api_keys_throttled_total Upstream keys in a per-account throttle window\n")
	out.WriteString("# TYPE opencode2api_keys_throttled_total gauge\n")
	fmt.Fprintf(&out, "opencode2api_keys_throttled_total{tier=\"zen\"} %d\n", zenThrottled)
	fmt.Fprintf(&out, "opencode2api_keys_throttled_total{tier=\"go\"} %d\n", goThrottled)
	fmt.Fprintf(&out, "# HELP opencode2api_keys_parked_total Upstream accounts parked out of rotation by quota exhaustion\n")
	out.WriteString("# TYPE opencode2api_keys_parked_total gauge\n")
	fmt.Fprintf(&out, "opencode2api_keys_parked_total{tier=\"zen\"} %d\n", zenParked)
	fmt.Fprintf(&out, "opencode2api_keys_parked_total{tier=\"go\"} %d\n", goParked)

	poolZenThrottled, poolGoThrottled := 0, 0
	if !g.zenNodes.ThrottleDeadline().IsZero() {
		poolZenThrottled = 1
	}
	if !g.goNodes.ThrottleDeadline().IsZero() {
		poolGoThrottled = 1
	}
	fmt.Fprintf(&out, "# HELP opencode2api_account_throttled 1 while the account-level rate-limit window is active\n")
	out.WriteString("# TYPE opencode2api_account_throttled gauge\n")
	fmt.Fprintf(&out, "opencode2api_account_throttled{tier=\"zen\"} %d\n", poolZenThrottled)
	fmt.Fprintf(&out, "opencode2api_account_throttled{tier=\"go\"} %d\n", poolGoThrottled)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(out.String()))
}

func snapshotKeys(g *Gateway) int {
	return g.zenNodes.Len() + g.goNodes.Len()
}

// escapeLabel quotes a label value per the expfmt specification. Model IDs are
// safe in practice, but this guards against unusual values.
func escapeLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return value
}
