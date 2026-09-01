package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type proxyTransport struct {
	index    int
	name     string
	client   *http.Client
	healthy  atomic.Bool
	checking atomic.Bool
}

type transportPool struct {
	items []*proxyTransport
}

func (p *transportPool) hasHealthy() bool {
	for _, proxy := range p.items {
		if proxy.healthy.Load() {
			return true
		}
	}
	return false
}

func (p *transportPool) healthCounts() (total, healthy int) {
	if p == nil {
		return 0, 0
	}
	for _, proxy := range p.items {
		if proxy.healthy.Load() {
			healthy++
		}
	}
	return len(p.items), healthy
}

func newTransportPool(proxies []string, cfg PerformanceConfig, responseHeaderTimeout time.Duration) (*transportPool, error) {
	p := &transportPool{items: make([]*proxyTransport, 0, len(proxies))}
	for _, raw := range proxies {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = cfg.MaxIdleConns
		transport.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
		transport.MaxConnsPerHost = cfg.MaxConnsPerHost
		transport.IdleConnTimeout = time.Duration(cfg.IdleConnTimeoutSeconds) * time.Second
		transport.ResponseHeaderTimeout = responseHeaderTimeout
		transport.ForceAttemptHTTP2 = true
		transport.DialContext = (&net.Dialer{
			Timeout:   time.Duration(cfg.ConnectTimeoutSeconds) * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
		if raw == "direct" {
			transport.Proxy = nil
		} else {
			u, err := url.Parse(raw)
			if err != nil {
				return nil, fmt.Errorf("parse proxy %s: %w", redactURL(raw), err)
			}
			transport.Proxy = http.ProxyURL(u)
		}
		proxy := &proxyTransport{index: len(p.items), name: raw, client: &http.Client{Transport: transport}}
		proxy.healthy.Store(true)
		p.items = append(p.items, proxy)
	}
	return p, nil
}

type proxyHealthResult struct {
	proxy      *proxyTransport
	err        error
	failed     bool
	wasHealthy bool
}

// CheckHealth concurrently rechecks only proxies already marked unhealthy.
// Healthy proxies are skipped before a check is claimed. Any HTTP response
// from the test URL proves that the route is reachable; only a timeout or
// connection refusal keeps the proxy unhealthy.
func (p *transportPool) CheckHealth(ctx context.Context, target string, timeout time.Duration) []proxyHealthResult {
	results := make(chan proxyHealthResult, len(p.items))
	checks := 0
	for _, proxy := range p.items {
		if proxy.healthy.Load() || !proxy.checking.CompareAndSwap(false, true) {
			continue
		}
		// A real request may have restored the proxy between the first health
		// read and claiming this check.
		if proxy.healthy.Load() {
			proxy.checking.Store(false)
			continue
		}
		checks++
		go func() {
			results <- p.checkClaimedProxy(ctx, proxy, target, timeout)
		}()
	}
	out := make([]proxyHealthResult, 0, checks)
	for range checks {
		out = append(out, <-results)
	}
	return out
}

// checkClaimedProxy performs a check after the caller has acquired checking.
func (p *transportPool) checkClaimedProxy(ctx context.Context, proxy *proxyTransport, target string, timeout time.Duration) proxyHealthResult {
	defer proxy.checking.Store(false)
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, target, nil)
	if err == nil {
		req.Header.Set("User-Agent", opencodeUserAgent())
		resp, requestErr := proxy.client.Do(req)
		err = requestErr
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
		}
	}
	result := proxyHealthResult{proxy: proxy, err: err, wasHealthy: proxy.healthy.Load()}
	if err == nil {
		result.wasHealthy = proxy.healthy.Swap(true)
	} else if isProxyFailure(err) {
		result.failed = true
		result.wasHealthy = proxy.healthy.Swap(false)
	}
	return result
}

// isProxyFailure deliberately recognizes only failures that say the proxy
// route is unavailable. HTTP responses and unrelated transport/protocol errors
// must not evict a proxy.
func isProxyFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

// upstreamNode keeps a key stable while allowing its proxy binding to change
// atomically when a proxy becomes unavailable.
type upstreamNode struct {
	key            string
	index          int
	preferredProxy int
	proxyIndex     atomic.Int64
	failures       atomic.Uint32
	cooldownUntil  atomic.Int64
	lastRemaining  atomic.Int64
	machineID      string
	vscodeMachine  string
	// Per-account throttle state, used only in multi-account mode where the
	// pool holds keys from distinct upstream accounts. A 429 on one account
	// throttles only its own key instead of the whole pool.
	throttleUntil atomic.Int64
	throttleHits  atomic.Uint32
	// parkedUntil marks a quota-exhausted account as out of rotation until its
	// free-tier quota window resets.
	parkedUntil atomic.Int64
}

type nodePool struct {
	nodes      []*upstreamNode
	transports *transportPool
	next       atomic.Uint64
	cooldown   time.Duration
	// maxCooldown caps the exponential backoff a single key can accumulate.
	// Zero means no cap. The fallback (go) tier sets this to the base
	// cooldown so a flaky relay cannot grow a key's cooldown past the
	// fallback window and silently disable the last-resort upstream.
	maxCooldown  time.Duration
	bindingsMu   sync.Mutex
	bindingCount []int
	// Account-level throttle. Upstreams commonly rate-limit every key of the
	// same account together, so key rotation alone cannot bypass a 429.
	// When distinct keys of the pool hit 429 inside the shared window the
	// whole pool enters a throttle window; requests then wait (backpressure)
	// instead of failing, and one probe request lifts it (half-open).
	throttleUntil atomic.Int64 // unixnano, 0 = not throttled
	throttleHits  atomic.Uint32
	last429Mu     sync.Mutex
	last429Seen   []int64 // unixnano per node index
	// multiAccount marks the pool as containing keys from distinct upstream
	// accounts. This changes throttle scope (per-key instead of per-pool) and
	// rotation (fair spread instead of session affinity).
	multiAccount bool
	// fairNext rotates the fair-rotation cursor independently of the model
	// refresh cursor, so background refreshes cannot perturb request spread.
	fairNext atomic.Uint64
}

// newNodePool distributes keys over proxies in round-robin order. When there
// are fewer keys than proxies, the remaining proxies are intentionally idle and
// can take over a key immediately if an active proxy fails.
func newNodePool(keys []string, transports *transportPool, cooldown time.Duration) (*nodePool, error) {
	if transports == nil || len(transports.items) == 0 {
		return nil, fmt.Errorf("at least one proxy transport is required")
	}
	pool := &nodePool{
		nodes:        make([]*upstreamNode, 0, len(keys)),
		transports:   transports,
		cooldown:     cooldown,
		bindingCount: make([]int, len(transports.items)),
	}
	if len(keys) == 0 {
		return pool, nil
	}
	for i, key := range keys {
		proxyIndex := i % len(transports.items)
		node := &upstreamNode{key: key, index: i, preferredProxy: proxyIndex}
		node.proxyIndex.Store(int64(proxyIndex))
		pool.nodes = append(pool.nodes, node)
		pool.bindingCount[proxyIndex]++
	}
	return pool, nil
}

// RestoreProxy moves keys that were originally assigned to a recovered proxy
// back to it. Proxies without an original key simply become healthy failover
// candidates again.
func (p *nodePool) RestoreProxy(recoveredProxy int) int {
	if p == nil || p.transports == nil || recoveredProxy < 0 || recoveredProxy >= len(p.transports.items) {
		return 0
	}
	p.bindingsMu.Lock()
	defer p.bindingsMu.Unlock()

	moved := 0
	for _, node := range p.nodes {
		current := int(node.proxyIndex.Load())
		if node.preferredProxy != recoveredProxy || current == recoveredProxy {
			continue
		}
		if current >= 0 && current < len(p.bindingCount) {
			p.bindingCount[current]--
		}
		p.bindingCount[recoveredProxy]++
		node.proxyIndex.Store(int64(recoveredProxy))
		node.failures.Store(0)
		node.cooldownUntil.Store(0)
		moved++
	}
	return moved
}

func (p *nodePool) Len() int { return len(p.nodes) }

// NodeByIndex returns the node at the given index, or nil if out of range.
func (p *nodePool) NodeByIndex(index int) *upstreamNode {
	if p == nil || index < 0 || index >= len(p.nodes) {
		return nil
	}
	return p.nodes[index]
}

// SetMultiAccount enables per-account throttle scope and fair rotation.
func (p *nodePool) SetMultiAccount(enabled bool) {
	p.multiAccount = enabled
}

// nodeBusyUntil returns the latest future deadline among the node's cooldown,
// throttle and park windows (zero when the node is eligible for rotation).
func (p *nodePool) nodeBusyUntil(node *upstreamNode, now int64) int64 {
	if node == nil {
		return 0
	}
	var until int64
	for _, candidate := range []*atomic.Int64{&node.cooldownUntil, &node.throttleUntil, &node.parkedUntil} {
		if value := candidate.Load(); value > until {
			until = value
		}
	}
	if until <= now {
		return 0
	}
	return until
}

// nodeEligible reports whether a node may serve traffic: no busy window and a
// healthy proxy binding.
func (p *nodePool) nodeEligible(node *upstreamNode, now int64) bool {
	if p == nil || node == nil {
		return false
	}
	if p.nodeBusyUntil(node, now) != 0 {
		return false
	}
	if p.transports != nil {
		if proxy := p.transports.items[int(node.proxyIndex.Load())]; proxy != nil && !proxy.healthy.Load() {
			return false
		}
	}
	return true
}

// activeNodesLocked orders the nodes that are currently eligible for rotation.
// start is the first node index tried; the order wraps around.
func (p *nodePool) activeNodesLocked(start int, now int64) []*upstreamNode {
	out := make([]*upstreamNode, 0, len(p.nodes))
	for offset := 0; offset < len(p.nodes); offset++ {
		index := (start + offset) % len(p.nodes)
		if node := p.nodes[index]; p.nodeEligible(node, now) {
			out = append(out, node)
		}
	}
	return out
}

func (p *nodePool) Proxy(node *upstreamNode) *proxyTransport {
	if p == nil || node == nil || p.transports == nil {
		return nil
	}
	index := int(node.proxyIndex.Load())
	if index < 0 || index >= len(p.transports.items) {
		return nil
	}
	return p.transports.items[index]
}

// SetFingerprints assigns a stable fake device identity to every node.
func (p *nodePool) SetFingerprints(store *fingerprintStore) {
	if p == nil || store == nil {
		return
	}
	for _, node := range p.nodes {
		node.machineID = store.ForKey(node.key).MachineID
		node.vscodeMachine = store.ForKey(node.key).VSCodeMachineID
	}
}

// MarkQuotaExceeded locks a node in COOLING_DOWN for quota exhaustion. Unlike
// MarkFailure it is not exponential and never clears early: the account really
// ran out of free quota, so we must not bounce back until the cooldown elapses.
func (p *nodePool) MarkQuotaExceeded(node *upstreamNode, cooldown time.Duration) {
	if node == nil {
		return
	}
	node.failures.Store(0)
	node.cooldownUntil.Store(time.Now().Add(cooldown).UnixNano())
}

// MarkAccountRejected cools a node for stable account-level rejections
// (401/403, e.g. "No payment method"). Unlike transient failures the
// condition will not clear on its own, so the node stays out of rotation for
// the full cooldown instead of being hammered every retry window.
func (p *nodePool) MarkAccountRejected(node *upstreamNode, cooldown time.Duration) {
	if node == nil {
		return
	}
	node.failures.Store(0)
	node.cooldownUntil.Store(time.Now().Add(cooldown).UnixNano())
}

// MarkNodeThrottled opens an exponential per-account throttle window on one
// key. Used in multi-account mode where the upstream rate-limits each account
// independently: the affected account backs off while the rest keep serving.
func (p *nodePool) MarkNodeThrottled(node *upstreamNode, cfg ThrottleConfig) {
	if node == nil {
		return
	}
	window := time.Duration(cfg.InitialSeconds) * time.Second
	if window <= 0 {
		window = 60 * time.Second
	}
	hits := node.throttleHits.Add(1)
	if max := time.Duration(cfg.MaxSeconds) * time.Second; max > 0 {
		if window*time.Duration(1<<min(hits-1, 8)) > max {
			window = max
		} else {
			window *= time.Duration(1 << min(hits-1, 8))
		}
	}
	node.throttleUntil.Store(time.Now().Add(window).UnixNano())
}

// ClearNodeThrottle lifts a per-account throttle after a successful probe.
func (p *nodePool) ClearNodeThrottle(node *upstreamNode) {
	if node == nil {
		return
	}
	node.throttleUntil.Store(0)
	node.throttleHits.Store(0)
}

// MarkQuotaParked parks a quota-exhausted account out of rotation until its
// free-tier quota window resets instead of only cooling it briefly. The park
// is not exponential and never clears early: the account really ran out of
// daily quota, so it must not re-enter rotation until the window elapses. Only
// the parked state is written, so Cooling()/Cooling metrics keep counting
// plain cooldowns and the parked gauge stays authoritative for parking.
func (p *nodePool) MarkQuotaParked(node *upstreamNode, window time.Duration) {
	if node == nil {
		return
	}
	node.failures.Store(0)
	node.parkedUntil.Store(time.Now().Add(window).UnixNano())
}

// ThrottleDeadline returns when the account-level throttle window ends
// (zero time when the pool is not throttled or the window already elapsed).
func (p *nodePool) ThrottleDeadline() time.Time {
	if p == nil {
		return time.Time{}
	}
	until := p.throttleUntil.Load()
	if until == 0 || until <= time.Now().UnixNano() {
		return time.Time{}
	}
	return time.Unix(0, until)
}

// MarkAccountThrottled enters an exponential throttle window. Repeated 429s
// double the window (60s, 120s, ...) up to ThrottleConfig.MaxSeconds, mirroring
// TCP congestion backoff so the upstream rate-limit window can elapse.
func (p *nodePool) MarkAccountThrottled(cfg ThrottleConfig) {
	window := time.Duration(cfg.InitialSeconds) * time.Second
	if window <= 0 {
		window = 60 * time.Second
	}
	hits := p.throttleHits.Add(1)
	if max := time.Duration(cfg.MaxSeconds) * time.Second; max > 0 {
		if window*time.Duration(1<<min(hits-1, 8)) > max {
			window = max
		} else {
			window *= time.Duration(1 << min(hits-1, 8))
		}
	}
	p.throttleUntil.Store(time.Now().Add(window).UnixNano())
}

// ClearAccountThrottle lifts the throttle after a successful probe request.
func (p *nodePool) ClearAccountThrottle() {
	p.throttleUntil.Store(0)
	p.throttleHits.Store(0)
}

// Record429 notes a 429 on one key. It returns true when at least threshold
// distinct keys of the pool hit 429 inside the shared window, which means the
// upstream rate-limits the whole account and key rotation cannot help.
func (p *nodePool) Record429(node *upstreamNode, window time.Duration, threshold int) bool {
	if p == nil || node == nil || threshold < 2 || window <= 0 {
		return false
	}
	now := time.Now().UnixNano()
	p.last429Mu.Lock()
	defer p.last429Mu.Unlock()
	if p.last429Seen == nil {
		p.last429Seen = make([]int64, len(p.nodes))
	}
	if node.index >= 0 && node.index < len(p.last429Seen) {
		p.last429Seen[node.index] = now
	}
	distinct := 0
	for _, seen := range p.last429Seen {
		if seen >= now-int64(window) {
			distinct++
		}
	}
	return distinct >= threshold
}

// keyStatus describes one upstream key for health endpoints. The pool's
// throttle state is reported on every key when active, since the throttle is
// account-wide.
type keyStatus struct {
	Index         int    `json:"index"`
	State         string `json:"state"`          // active | cooling | throttled
	CooldownUntil int64  `json:"cooldown_until"` // unixnano, 0 = none
}

// StatusSnapshot reports per-key state for healthz/metrics consumers.
func (p *nodePool) StatusSnapshot() []keyStatus {
	if p == nil {
		return nil
	}
	now := time.Now().UnixNano()
	poolThrottled := !p.multiAccount && p.throttleUntil.Load() > now
	out := make([]keyStatus, 0, len(p.nodes))
	for _, node := range p.nodes {
		state := p.nodeState(node, now, poolThrottled)
		out = append(out, keyStatus{Index: node.index, State: state, CooldownUntil: p.nodeBusyUntil(node, now)})
	}
	return out
}

// nodeState reports the canonical state of one node for health consumers.
func (p *nodePool) nodeState(node *upstreamNode, now int64, poolThrottled bool) string {
	if p.multiAccount {
		if node.parkedUntil.Load() > now {
			return "parked"
		}
		if node.throttleUntil.Load() > now {
			return "throttled"
		}
		if node.cooldownUntil.Load() > now {
			return "cooling"
		}
		return "active"
	}
	if poolThrottled {
		return "throttled"
	}
	if node.cooldownUntil.Load() > now {
		return "cooling"
	}
	return "active"
}

// EarliestBusy returns the soonest expiry across all per-node busy windows
// (cooldown, throttle, park). Zero time when no node is busy.
func (p *nodePool) EarliestBusy() time.Time {
	if p == nil {
		return time.Time{}
	}
	now := time.Now().UnixNano()
	var earliest int64
	for _, node := range p.nodes {
		if until := p.nodeBusyUntil(node, now); until != 0 {
			if earliest == 0 || until < earliest {
				earliest = until
			}
		}
	}
	if earliest == 0 {
		return time.Time{}
	}
	return time.Unix(0, earliest)
}

// EarliestCooldown is the legacy name for EarliestBusy, kept so existing call
// sites and tests read naturally regardless of which window is busy.
func (p *nodePool) EarliestCooldown() time.Time {
	return p.EarliestBusy()
}

// ObserveRateLimit records the upstream x-ratelimit-remaining header. When
// proactive limiting is enabled and the remaining budget is about to run out,
// the node is cooled briefly so the next request is routed elsewhere before
// the upstream starts returning 429s.
func (p *nodePool) ObserveRateLimit(node *upstreamNode, header http.Header, proactive bool, rotateAt int) {
	if node == nil || header == nil {
		return
	}
	raw := header.Get("x-ratelimit-remaining")
	if raw == "" {
		raw = header.Get("x-ratelimit-remaining-requests")
	}
	remaining := int64(-1)
	if value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
		remaining = value
	}
	node.lastRemaining.Store(remaining)
	if !proactive || remaining < 0 {
		return
	}
	if remaining <= int64(rotateAt) {
		// Short idle so Next() prefers another node, but expires quickly.
		node.cooldownUntil.Store(time.Now().Add(15 * time.Second).UnixNano())
	}
}

// Cooling returns how many nodes are currently in COOLING_DOWN (any reason).
func (p *nodePool) Cooling() int {
	if p == nil {
		return 0
	}
	now := time.Now().UnixNano()
	count := 0
	for _, node := range p.nodes {
		if node.cooldownUntil.Load() > now {
			count++
		}
	}
	return count
}

// ActiveNodeCount returns the number of nodes not currently cooled down.
func (p *nodePool) ActiveNodeCount() int {
	if p == nil {
		return 0
	}
	now := time.Now().UnixNano()
	count := 0
	for _, node := range p.nodes {
		if node.cooldownUntil.Load() <= now {
			count++
		}
	}
	return count
}

// ThrottledKeyCount returns how many nodes are currently in a per-account
// throttle window (multi-account mode).
func (p *nodePool) ThrottledKeyCount() int {
	if p == nil {
		return 0
	}
	now := time.Now().UnixNano()
	count := 0
	for _, node := range p.nodes {
		if node.throttleUntil.Load() > now {
			count++
		}
	}
	return count
}

// Parked returns how many nodes are currently parked out of rotation by quota
// exhaustion (multi-account mode).
func (p *nodePool) Parked() int {
	if p == nil {
		return 0
	}
	now := time.Now().UnixNano()
	count := 0
	for _, node := range p.nodes {
		if node.parkedUntil.Load() > now {
			count++
		}
	}
	return count
}

// ActiveOrder returns the nodes that are currently eligible for rotation,
// ordered from a stable cursor position for the supplied affinity key. In
// single-account mode eligibility means not cooling down; in multi-account
// mode it also excludes per-account throttled and parked nodes. When every
// node is busy the returned slice is empty.
func (p *nodePool) ActiveOrder(affinity string) []*upstreamNode {
	if p == nil || len(p.nodes) == 0 {
		return nil
	}
	start := 0
	if affinity != "" && !p.multiAccount {
		hash := fnv.New64a()
		_, _ = hash.Write([]byte(affinity))
		start = int(hash.Sum64() % uint64(len(p.nodes)))
	}
	return p.activeNodesLocked(start, time.Now().UnixNano())
}

// ActiveOrderFair returns the eligible nodes ordered from a rotating cursor,
// so consecutive requests spread across accounts. Used in multi-account mode
// where session affinity must not pin a conversation to one account.
func (p *nodePool) ActiveOrderFair() []*upstreamNode {
	if p == nil || len(p.nodes) == 0 {
		return nil
	}
	start := int((p.fairNext.Add(1) - 1) % uint64(len(p.nodes)))
	return p.activeNodesLocked(start, time.Now().UnixNano())
}

// RebindProxy moves every key currently using failedProxy to the least-loaded
// healthy proxy. Empty proxies are selected in configuration order; otherwise
// one of the least-loaded proxies is chosen at random. If every alternative is
// currently unhealthy, it still attempts the least-loaded alternative.
func (p *nodePool) RebindProxy(failedProxy int) int {
	if p == nil || p.transports == nil || len(p.transports.items) < 2 || failedProxy < 0 || failedProxy >= len(p.transports.items) {
		return 0
	}
	p.bindingsMu.Lock()
	defer p.bindingsMu.Unlock()

	moved := 0
	for _, node := range p.nodes {
		if int(node.proxyIndex.Load()) != failedProxy {
			continue
		}
		target := p.replacementLocked(failedProxy, true)
		if target < 0 {
			target = p.replacementLocked(failedProxy, false)
		}
		if target < 0 {
			continue
		}
		p.bindingCount[failedProxy]--
		p.bindingCount[target]++
		node.proxyIndex.Store(int64(target))
		node.failures.Store(0)
		node.cooldownUntil.Store(0)
		moved++
	}
	return moved
}

func (p *nodePool) replacementLocked(failedProxy int, healthyOnly bool) int {
	minimum := int(^uint(0) >> 1)
	candidates := make([]int, 0, len(p.transports.items)-1)
	for i := range p.transports.items {
		if i == failedProxy || healthyOnly && !p.transports.items[i].healthy.Load() {
			continue
		}
		count := p.bindingCount[i]
		if count == 0 {
			return i
		}
		if count < minimum {
			minimum = count
			candidates = candidates[:0]
			candidates = append(candidates, i)
		} else if count == minimum {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return -1
	}
	return candidates[rand.IntN(len(candidates))]
}

type nodeCursor struct {
	pool *nodePool
	next int
}

// Cursor reserves a different starting node for each concurrent request.
// Selection is delayed until Next, so a node marked failed by the preceding
// attempt is immediately skipped. Both Cursor and Next allocate no memory.
func (p *nodePool) Cursor() nodeCursor {
	if len(p.nodes) == 0 {
		return nodeCursor{pool: p}
	}
	return nodeCursor{pool: p, next: int((p.next.Add(1) - 1) % uint64(len(p.nodes)))}
}

// CursorFor returns a cursor whose first choice is stable for the supplied
// affinity key. This keeps every turn in a conversation on the same upstream
// key while retaining Next's cooldown-aware failover behavior. Empty affinity
// keys keep the round-robin behavior used by background tasks.
func (p *nodePool) CursorFor(affinity string) nodeCursor {
	if affinity == "" || len(p.nodes) == 0 {
		return p.Cursor()
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(affinity))
	return nodeCursor{pool: p, next: int(hash.Sum64() % uint64(len(p.nodes)))}
}

func (c *nodeCursor) Next() *upstreamNode {
	if c.pool == nil || len(c.pool.nodes) == 0 {
		return nil
	}
	now := time.Now().UnixNano()
	choice := -1
	var earliest int64
	for offset := 0; offset < len(c.pool.nodes); offset++ {
		i := (c.next + offset) % len(c.pool.nodes)
		until := c.pool.nodeBusyUntil(c.pool.nodes[i], now)
		if until <= 0 {
			choice = i
			break
		}
		if choice == -1 || until < earliest {
			choice, earliest = i, until
		}
	}
	if choice < 0 {
		return nil
	}
	c.next = (choice + 1) % len(c.pool.nodes)
	return c.pool.nodes[choice]
}

func (p *nodePool) MarkSuccess(node *upstreamNode) {
	if node == nil {
		return
	}
	node.failures.Store(0)
	node.cooldownUntil.Store(0)
	if p.multiAccount {
		// A successful probe lifts a per-account throttle. Parked accounts
		// never receive requests until their park window elapses, so a success
		// here means the park had already expired and the account rejoined
		// rotation on its own.
		p.ClearNodeThrottle(node)
	}
}

func (p *nodePool) MarkFailure(node *upstreamNode, resp *http.Response, err error) {
	if err == nil && resp != nil && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
		return
	}
	failures := node.failures.Add(1)
	multiplier := time.Duration(1 << min(failures-1, 3))
	delay := p.cooldown * multiplier
	// Respect the upstream's Retry-After hint...
	if resp != nil {
		if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > delay {
			delay = retryAfter
		}
	}
	// ...but always honour the pool-wide cooldown cap so a large Retry-After
	// (or an upstream with no cap) cannot permanently disable a key.
	if p.maxCooldown > 0 && delay > p.maxCooldown {
		delay = p.maxCooldown
	}
	node.cooldownUntil.Store(time.Now().Add(delay).UnixNano())
}

func parseRetryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(time.Until(when), 0)
	}
	return 0
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}
