package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

type Config struct {
	Listen       string            `json:"listen"`
	ServerKeys   []string          `json:"server_keys"`
	ZenKeys      []string          `json:"zen_keys"`
	GoKeys       []string          `json:"go_keys"`
	Proxies      []string          `json:"proxies"`
	Upstream     UpstreamConfig    `json:"upstream"`
	UpstreamMode UpstreamMode      `json:"upstream_mode"`
	Retry        RetryConfig       `json:"retry"`
	Models       ModelsConfig      `json:"models"`
	Performance  PerformanceConfig `json:"performance"`
	Logging      LoggingConfig     `json:"logging"`
	Stats        StatsConfig       `json:"stats"`
	Prefer       Tier              `json:"prefer"`
	Sanitize     SanitizeConfig    `json:"sanitize"`
	Failover     FailoverConfig    `json:"failover"`
	Fingerprint  FingerprintConfig `json:"fingerprint"`
	RateLimit    RateLimitConfig   `json:"rate_limit"`
}

type UpstreamConfig struct {
	Zen string `json:"zen"`
	Go  string `json:"go"`
}

// UpstreamMode selects how the gateway talks to the configured upstream.
//
//   - ModeOpenCode (default): the upstream is opencode.ai and the gateway sends
//     the opencode client headers, treats 401 as "no payment method", and
//     fetches the model catalog from opencode's /v1/models endpoint.
//   - ModeOpenAI: the upstream is any OpenAI-compatible endpoint (e.g. Google
//     AI Studio / Gemini at https://generativelanguage.googleapis.com/v1beta/openai).
//     The gateway sends only standard headers, allows gemini-* models, and uses
//     the model list from models.static (or the upstream's /models endpoint).
type UpstreamMode string

const (
	ModeOpenCode UpstreamMode = "opencode"
	ModeOpenAI   UpstreamMode = "openai"
)

func (m UpstreamMode) isOpenAI() bool { return m == ModeOpenAI }

type RetryConfig struct {
	MaxAttempts    int `json:"max_attempts"`
	TimeoutSeconds int `json:"timeout_seconds"`
}

type ModelsConfig struct {
	RefreshSeconds int               `json:"refresh_seconds"`
	Protocols      map[string]string `json:"protocols"`
	// Static is a fixed model list used as the catalog instead of fetching it
	// from the upstream. Required for ModeOpenAI upstreams whose /models
	// endpoint is not OpenAI-compatible (e.g. Google AI Studio).
	Static []string `json:"static"`
	// StaticGo is the static catalog for the second upstream (the "go" tier,
	// e.g. an OpenAI/Anthropic relay). Models listed here route to
	// upstream.go with go_keys, keeping failover and metrics per upstream.
	StaticGo []string `json:"static_go"`
}

type LoggingConfig struct {
	Level string `json:"level"`
}

// StatsConfig controls usage statistics. AuditFile enables durable JSONL
// audit logging of every request; leave empty to keep stats in memory only.
type StatsConfig struct {
	AuditFile string `json:"audit_file"`
}

type PerformanceConfig struct {
	MaxIdleConns           int `json:"max_idle_conns"`
	MaxIdleConnsPerHost    int `json:"max_idle_conns_per_host"`
	MaxConnsPerHost        int `json:"max_conns_per_host"`
	IdleConnTimeoutSeconds int `json:"idle_conn_timeout_seconds"`
	ConnectTimeoutSeconds  int `json:"connect_timeout_seconds"`
	FailureCooldownSeconds int `json:"failure_cooldown_seconds"`
}

// SanitizeConfig cleans client request bodies before forwarding upstream.
// StripFreeSuffix is disabled by default: on opencode.ai the "-free" suffix is
// the free model's identity, and stripping it routes to the paid model, which
// accounts without a payment method reject with 401. Use model_aliases for
// remapping instead.
type SanitizeConfig struct {
	Enabled         bool              `json:"enabled"`
	StripFreeSuffix bool              `json:"strip_free_suffix"`
	ModelAliases    map[string]string `json:"model_aliases"`
}

// FailoverConfig controls the silent quota-failover behavior.
//
// Failover alone cannot bypass upstream limits that are shared across every
// key of the same account (429 "rate limit exceeded"). Throttle adds an
// account-level circuit breaker: when enough distinct keys hit 429 inside the
// shared window the whole pool enters a throttle window and requests wait
// (backpressure) instead of failing, probing one request when the window
// expires (half-open), like TCP congestion control.
type FailoverConfig struct {
	Enabled                bool           `json:"enabled"`
	QuotaCooldownMinutes   int            `json:"quota_cooldown_minutes"`
	TreatGeneric429AsQuota bool           `json:"treat_generic_429_as_quota"`
	Throttle               ThrottleConfig `json:"throttle"`
	// MultiAccount marks the pool as containing keys from distinct upstream
	// accounts. Upstreams then rate-limit per account, so 429s must cool only
	// the affected key instead of throttling the whole pool, and session
	// affinity must not pin every turn of a conversation to one account.
	MultiAccount bool `json:"multi_account"`
	// QuotaParkMinutes parks a quota-exhausted account out of rotation for the
	// remainder of the free-tier quota window instead of the short cooldown.
	// Zero disables parking and keeps the existing quota cooldown behaviour.
	QuotaParkMinutes int `json:"quota_park_minutes"`
}

// ThrottleConfig tunes the account-level rate-limit circuit breaker.
type ThrottleConfig struct {
	InitialSeconds     int `json:"initial_seconds"`      // first throttle window
	MaxSeconds         int `json:"max_seconds"`          // window cap after repeated 429s
	Shared429Threshold int `json:"shared_429_threshold"` // distinct keys 429ing inside the window that prove shared limits
	MaxWaitSeconds     int `json:"max_wait_seconds"`     // how long a request holds (backpressure) before giving up
}

// FingerprintConfig gives every account key a stable fake device identity.
type FingerprintConfig struct {
	Enabled     bool   `json:"enabled"`
	PersistFile string `json:"persist_file"`
}

// RateLimitConfig lets the scheduler pre-empt rate limiting using upstream
// x-ratelimit-remaining headers.
type RateLimitConfig struct {
	Enabled           bool `json:"enabled"`
	Proactive         bool `json:"proactive"`
	RotateAtRemaining int  `json:"rotate_at_remaining"`
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	cfg := Config{
		Listen:       "127.0.0.1:8080",
		Proxies:      []string{"direct"},
		Upstream:     UpstreamConfig{Zen: "https://opencode.ai/zen", Go: "https://opencode.ai/zen/go"},
		UpstreamMode: ModeOpenCode,
		Retry:        RetryConfig{MaxAttempts: 3, TimeoutSeconds: 300},
		Models:       ModelsConfig{RefreshSeconds: 300, Protocols: map[string]string{}},
		Performance:  PerformanceConfig{MaxIdleConns: 2048, MaxIdleConnsPerHost: 256, MaxConnsPerHost: 0, IdleConnTimeoutSeconds: 120, ConnectTimeoutSeconds: 5, FailureCooldownSeconds: 15},
		Logging:      LoggingConfig{Level: "info"},
		Stats:        StatsConfig{},
		Prefer:       TierGo,
		Sanitize: SanitizeConfig{
			Enabled:         true,
			StripFreeSuffix: false,
			ModelAliases:    map[string]string{},
		},
		Failover: FailoverConfig{
			Enabled:                true,
			QuotaCooldownMinutes:   30,
			TreatGeneric429AsQuota: false,
			Throttle: ThrottleConfig{
				InitialSeconds:     60,
				MaxSeconds:         600,
				Shared429Threshold: 2,
				MaxWaitSeconds:     60,
			},
		},
		Fingerprint: FingerprintConfig{
			Enabled:     true,
			PersistFile: "fingerprints.json",
		},
		RateLimit: RateLimitConfig{
			Enabled:           true,
			Proactive:         true,
			RotateAtRemaining: 2,
		},
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	trimList(&cfg.ServerKeys)
	trimList(&cfg.ZenKeys)
	trimList(&cfg.GoKeys)
	trimList(&cfg.Proxies)
	if cfg.Prefer != TierZen && cfg.Prefer != TierGo {
		return Config{}, errors.New("prefer must be \"zen\" or \"go\"")
	}
	if cfg.UpstreamMode != "" && cfg.UpstreamMode != ModeOpenCode && cfg.UpstreamMode != ModeOpenAI {
		return Config{}, errors.New("upstream_mode must be \"opencode\" or \"openai\"")
	}
	if cfg.UpstreamMode.isOpenAI() && strings.TrimSpace(cfg.Upstream.Zen) == "" {
		return Config{}, errors.New("upstream.zen must point at the OpenAI-compatible base URL when upstream_mode is \"openai\"")
	}
	if cfg.Listen == "" {
		return Config{}, errors.New("listen must not be empty")
	}
	if len(cfg.ServerKeys) == 0 {
		return Config{}, errors.New("server_keys must contain at least one local key")
	}
	if len(cfg.ZenKeys) == 0 && len(cfg.GoKeys) == 0 {
		return Config{}, errors.New("zen_keys or go_keys must contain at least one upstream key")
	}
	if len(cfg.Proxies) == 0 {
		cfg.Proxies = []string{"direct"}
	}
	if cfg.Retry.MaxAttempts < 1 {
		return Config{}, errors.New("retry.max_attempts must be at least 1")
	}
	if cfg.Retry.TimeoutSeconds < 1 {
		return Config{}, errors.New("retry.timeout_seconds must be at least 1")
	}
	if cfg.Models.RefreshSeconds < 1 {
		return Config{}, errors.New("models.refresh_seconds must be at least 1")
	}
	if cfg.Performance.MaxIdleConns < 1 || cfg.Performance.MaxIdleConnsPerHost < 1 || cfg.Performance.MaxConnsPerHost < 0 || cfg.Performance.IdleConnTimeoutSeconds < 1 || cfg.Performance.ConnectTimeoutSeconds < 1 || cfg.Performance.FailureCooldownSeconds < 1 {
		return Config{}, errors.New("performance values must be positive (max_conns_per_host may be zero for unlimited)")
	}
	for _, raw := range cfg.Proxies {
		if raw == "direct" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return Config{}, fmt.Errorf("invalid proxy URL %q", redactURL(raw))
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return Config{}, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
		}
	}
	if cfg.Failover.QuotaParkMinutes < 0 {
		return Config{}, errors.New("failover.quota_park_minutes must not be negative")
	}
	for model, protocol := range cfg.Models.Protocols {
		if model == "" || !validProtocol(Protocol(protocol)) {
			return Config{}, fmt.Errorf("models.protocols contains invalid mapping %q: %q", model, protocol)
		}
	}
	return cfg, nil
}

func trimList(items *[]string) {
	out := (*items)[:0]
	for _, item := range *items {
		if value := strings.TrimSpace(item); value != "" {
			out = append(out, value)
		}
	}
	*items = out
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid>"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	return u.String()
}
