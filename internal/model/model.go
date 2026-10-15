package model

import "time"

type Provider struct {
	Name    string
	BaseURL string

	Fallback       []string
	HealthCheck    bool
	HealthInterval time.Duration
	RPM            int
	Timeout        time.Duration
	Keys           []Key

	ProxyURL string

	Disabled bool

	Headers map[string]string

	ExcludedModels []string

	Catalog bool

	RequestRetry *int

	DisableCooling bool
}

type Key struct {
	ID     string
	Secret string
	Ref    string

	RPM            int
	MaxRequests    int
	Window         time.Duration
	MaxConcurrent  int
	CostLimitCents int64

	CostLimitMicros int64

	Weight   int
	Priority int

	Headers map[string]string

	ProxyURL string

	RequestRetry *int

	DisableCooling bool

	ErrorRules []ErrorRule
}

type ErrorRule struct {
	Status      int
	Match       []string
	MatchRegexp []string
	Action      string
}

type Alias struct {
	Name    string
	Targets []Target

	DisplayName string

	ExcludedModels []string
}

type Target struct {
	Provider       string
	Model          string
	ContextWindow  int
	SupportsTools  bool
	SupportsVision bool
	SupportsStream bool

	PriceInputPerM  float64
	PriceOutputPerM float64

	ForceMapping bool

	DisplayName string
}

type RequestMeta struct {
	ID            string    `json:"id"`
	StartedAt     time.Time `json:"started_at"`
	DurMillis     int64     `json:"dur_ms"`
	Provider      string    `json:"provider"`
	Key           string    `json:"key"`
	Model         string    `json:"model"`
	UpstreamModel string    `json:"upstream_model"`
	Stream        bool      `json:"stream"`
	Cached        bool      `json:"cached"`

	Coalesced        bool   `json:"coalesced"`
	Status           string `json:"status"`
	RouteReason      string `json:"route_reason"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`

	CostMicros int64  `json:"cost_micros"`
	Err        string `json:"err"`
}
