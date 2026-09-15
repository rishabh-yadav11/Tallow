// Package model holds the shared runtime types consumed across the gateway:
// providers, keys, aliases, and per-request metadata. Config (TOML) is
// deserialized separately and converted into these runtime values.
package model

import "time"

// Provider is one OpenAI-compatible upstream endpoint. Tallow never hardcodes
// vendors; anything with a key + base URL and the chat-completions contract
// works.
type Provider struct {
	Name    string
	BaseURL string
	// Fallback is an ordered list of provider names to try when this provider
	// is unavailable or has no usable key.
	Fallback       []string
	HealthCheck    bool
	HealthInterval time.Duration
	RPM            int // optional aggregate provider-level RPM cap.
	Timeout        time.Duration
	Keys           []Key
}

// Key is a single account/credential within a provider, plus its hard limits.
type Key struct {
	ID     string
	Secret string // decrypted at runtime; never persisted plaintext.
	Ref    string // keystore reference resolving the ciphertext.

	// Budgets. Zero means unlimited.
	RPM            int           // fixed-window requests per minute.
	MaxRequests    int           // hard cap within Window.
	Window         time.Duration // window for MaxRequests.
	MaxConcurrent  int           // per-key in-flight cap.
	CostLimitCents int64         // cumulative cost cap.
}

// Alias maps a client-facing model name to an ordered list of provider
// targets. The order is the fallback order for failover.
type Alias struct {
	Name    string
	Targets []Target
}

// Target binds an alias to a specific provider + upstream model string, with
// manually-declared capabilities and pricing. Auto-detection is explicitly out
// of scope.
type Target struct {
	Provider       string
	Model          string // the upstream model string to actually send.
	ContextWindow  int
	SupportsTools  bool
	SupportsVision bool
	SupportsStream bool

	// Pricing, USD per 1M tokens. 0 = unknown, reported as $0.
	PriceInputPerM  float64
	PriceOutputPerM float64
}

// RequestMeta is the per-request record: what routed where, cost, tokens,
// status, and the reason a routing decision fired. Used by both the retention
// store and (when enabled) live observability. JSON tags match the admin API
// wire contract (snake_case).
type RequestMeta struct {
	ID               string    `json:"id"`
	StartedAt        time.Time `json:"started_at"`
	DurMillis        int64     `json:"dur_ms"`
	Provider         string    `json:"provider"`
	Key              string    `json:"key"`
	Model            string    `json:"model"`
	UpstreamModel    string    `json:"upstream_model"`
	Stream           bool      `json:"stream"`
	Cached           bool      `json:"cached"`
	Status           string    `json:"status"`
	RouteReason      string    `json:"route_reason"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	CostCents        int64     `json:"cost_cents"`
	Err              string    `json:"err"`
}
