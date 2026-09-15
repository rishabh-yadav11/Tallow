// Package main implements tallowctl, the terminal dashboard for the Tallow
// LLM gateway. It talks only to the gateway's admin control interface over a
// local Unix-socket HTTP API and never reaches into gateway internals.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	pollInterval = 2 * time.Second
	logLimit     = 20
	shownLogs    = 15
)

// runTUI runs the Bubble Tea dashboard for the gateway admin API served over
// the given Unix domain socket until the user quits (q / Esc / Ctrl-C).
func runTUI(adminSocket string) error {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", adminSocket)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}

	m := initialModel(client, adminSocket)
	p := tea.NewProgram(&m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// ---- data shapes ----
//
// Most admin endpoints (providers, aliases, health, cache, config) are served
// from Go view types that carry snake_case json tags. The raw metrics and
// request-log records, however, are marshalled from tag-less structs and thus
// hit the wire with Go's default PascalCase field names (e.g. UptimeSeconds,
// DurMillis, PromptTokens). Those are decoded via a tolerant object view so the
// dashboard works against either casing.

type healthResp struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

type providerStat struct {
	Requests  int64
	Errors    int64
	CostCents int64
}

func (p *providerStat) UnmarshalJSON(data []byte) error {
	f := parseFlex(data)
	p.Requests = f.num("Requests", "requests")
	p.Errors = f.num("Errors", "errors")
	p.CostCents = f.num("CostCents", "cost_cents")
	return nil
}

type metricsResp struct {
	Enabled          bool
	UptimeSeconds    int64
	Total            int64
	Errors           int64
	Cached           int64
	PromptTokens     int64
	CompletionTokens int64
	CostCents        int64
	LatencyP50Ms     int64
	LatencyP95Ms     int64
	ByProvider       map[string]providerStat
}

func (m *metricsResp) UnmarshalJSON(data []byte) error {
	f := parseFlex(data)
	m.Enabled = f.boolean("Enabled", "enabled")
	m.UptimeSeconds = f.num("UptimeSeconds", "uptime_seconds")
	m.Total = f.num("Total", "total")
	m.Errors = f.num("Errors", "errors")
	m.Cached = f.num("Cached", "cached")
	m.PromptTokens = f.num("PromptTokens", "prompt_tokens")
	m.CompletionTokens = f.num("CompletionTokens", "completion_tokens")
	m.CostCents = f.num("CostCents", "cost_cents")
	m.LatencyP50Ms = f.num("LatencyP50Ms", "latency_p50_ms")
	m.LatencyP95Ms = f.num("LatencyP95Ms", "latency_p95_ms")
	if raw := f.get("ByProvider", "by_provider"); raw != nil {
		_ = json.Unmarshal(raw, &m.ByProvider)
	}
	return nil
}

type keyView struct {
	ID           string `json:"id"`
	Ref          string `json:"ref"`
	RPM          int    `json:"rpm"`
	Health       string `json:"health"`
	RPMRemaining int    `json:"rpm_remaining"`
	Inflight     int    `json:"inflight"`
	MaxInflight  int    `json:"max_inflight"`
	CostCents    int64  `json:"cost_cents"`
	CostLimit    int64  `json:"cost_limit_cents"`
}

type providerView struct {
	Name    string    `json:"name"`
	BaseURL string    `json:"base_url"`
	RPM     int       `json:"rpm"`
	Health  string    `json:"health"`
	Keys    []keyView `json:"keys"`
}

type targetView struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type aliasView struct {
	Name    string       `json:"name"`
	Targets []targetView `json:"targets"`
}

type logEntry struct {
	ID               string
	StartedAt        string
	DurMillis        int64
	Provider         string
	Key              string
	Model            string
	UpstreamModel    string
	Stream           bool
	Cached           bool
	Status           string
	RouteReason      string
	PromptTokens     int
	CompletionTokens int
	CostCents        int64
	Err              string
}

func (l *logEntry) UnmarshalJSON(data []byte) error {
	f := parseFlex(data)
	l.ID = f.str("ID", "id")
	l.StartedAt = f.str("StartedAt", "started_at")
	l.DurMillis = f.num("DurMillis", "dur_millis", "dur_ms")
	l.Provider = f.str("Provider", "provider")
	l.Key = f.str("Key", "key")
	l.Model = f.str("Model", "model")
	l.UpstreamModel = f.str("UpstreamModel", "upstream_model")
	l.Stream = f.boolean("Stream", "stream")
	l.Cached = f.boolean("Cached", "cached")
	l.Status = f.str("Status", "status")
	l.RouteReason = f.str("RouteReason", "route_reason")
	l.PromptTokens = int(f.num("PromptTokens", "prompt_tokens"))
	l.CompletionTokens = int(f.num("CompletionTokens", "completion_tokens"))
	l.CostCents = f.num("CostCents", "cost_cents")
	l.Err = f.str("Err", "err")
	return nil
}

type cacheStats struct {
	Hit   int64 `json:"hit"`
	Miss  int64 `json:"miss"`
	Evict int64 `json:"evict"`
	Size  int64 `json:"size"`
}

type configResp struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

// snapshot holds everything the dashboard renders for one poll.
type snapshot struct {
	health       healthResp
	metrics      metricsResp
	providers    []providerView
	aliases      []aliasView
	healthStates map[string]string
	logs         []logEntry
	cache        cacheStats
	config       configResp
}

// ---- tolerant JSON decoding ----

type flexObject map[string]json.RawMessage

func parseFlex(data []byte) flexObject {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return flexObject{}
	}
	return flexObject(m)
}

// get returns the value for the first candidate key present, matching keys
// case- and separator-insensitively as a fallback.
func (f flexObject) get(candidates ...string) json.RawMessage {
	for _, c := range candidates {
		if v, ok := f[c]; ok {
			return v
		}
	}
	norm := make(map[string]json.RawMessage, len(f))
	for k, v := range f {
		norm[normalizeKey(k)] = v
	}
	for _, c := range candidates {
		if v, ok := norm[normalizeKey(c)]; ok {
			return v
		}
	}
	return nil
}

func (f flexObject) num(candidates ...string) int64 {
	v := f.get(candidates...)
	if v == nil {
		return 0
	}
	var n int64
	_ = json.Unmarshal(v, &n)
	return n
}

func (f flexObject) str(candidates ...string) string {
	v := f.get(candidates...)
	if v == nil {
		return ""
	}
	var s string
	_ = json.Unmarshal(v, &s)
	return s
}

func (f flexObject) boolean(candidates ...string) bool {
	v := f.get(candidates...)
	if v == nil {
		return false
	}
	var b bool
	if err := json.Unmarshal(v, &b); err == nil {
		return b
	}
	var n int64
	if err := json.Unmarshal(v, &n); err == nil {
		return n != 0
	}
	return false
}

func normalizeKey(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_' || c == '-':
			continue
		case 'A' <= c && c <= 'Z':
			c += 'a' - 'A'
		}
		b = append(b, c)
	}
	return string(b)
}

// ---- model ----

type tickMsg struct{}

type fetchResult struct {
	snap *snapshot
	err  error
}

type model struct {
	client *http.Client
	socket string
	snap   *snapshot
	err    error
	width  int
	height int
}

func initialModel(client *http.Client, socket string) model {
	return model{client: client, socket: socket}
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.tick(), m.fetch())
}

func (m *model) tick() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *model) fetch() tea.Cmd {
	return func() tea.Msg {
		snap, err := fetchAll(m.client)
		return fetchResult{snap: snap, err: err}
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tickMsg:
		return m, tea.Batch(m.tick(), m.fetch())
	case fetchResult:
		if msg.err != nil {
			m.err = msg.err
			m.snap = nil
		} else {
			m.err = nil
			m.snap = msg.snap
		}
	}
	return m, nil
}

// ---- fetch ----

func getJSON(client *http.Client, path string, out any) error {
	resp, err := client.Get("http://tallow" + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// fetchAll probes /health first (the liveness signal); if that fails the
// gateway is unreachable. Remaining endpoints are best-effort so a single bad
// endpoint doesn't blank the whole dashboard.
func fetchAll(client *http.Client) (*snapshot, error) {
	s := &snapshot{}
	if err := getJSON(client, "/health", &s.health); err != nil {
		return nil, err
	}

	_ = getJSON(client, "/metrics", &s.metrics)
	_ = getJSON(client, "/providers", &s.providers)
	_ = getJSON(client, "/aliases", &s.aliases)
	var budgets []json.RawMessage
	_ = getJSON(client, "/budgets", &budgets)
	_ = getJSON(client, "/health-states", &s.healthStates)
	_ = getJSON(client, fmt.Sprintf("/logs?limit=%d", logLimit), &s.logs)
	_ = getJSON(client, "/cache", &s.cache)
	_ = getJSON(client, "/config", &s.config)

	return s, nil
}

// ---- rendering ----

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00d5ff"))
	sectionStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#b388ff"))
	labelStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	okStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00c853"))
	badStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ff5252"))
	warnStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffd740"))
	mutedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

func healthColor(state string) string {
	switch state {
	case "closed":
		return "#00c853" // circuit closed: healthy
	case "open":
		return "#ff5252" // circuit tripped: unhealthy
	case "half-open":
		return "#ffd740" // testing
	default:
		return "245"
	}
}

func statusColor(status string) string {
	switch status {
	case "ok", "cached":
		return "#00c853"
	case "error":
		return "#ff5252"
	default:
		return "245"
	}
}

// rpmStr renders an RPM remaining value; negative means "unlimited".
func rpmStr(v int) string {
	if v < 0 {
		return "∞"
	}
	return strconv.Itoa(v)
}

func dollars(cents int64) string {
	return fmt.Sprintf("$%.2f", float64(cents)/100.0)
}

func fmtUptime(sec int64) string {
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	d := sec / 86400
	h := (sec % 86400) / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	if d > 0 {
		return fmt.Sprintf("%dd%dh", d, h)
	}
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dm%ds", m, s)
}

func (m *model) View() string {
	if m.err != nil {
		return m.unreachableView()
	}
	if m.snap == nil {
		return titleStyle.Render("tallowctl") + "\n\nconnecting…\n"
	}
	return renderDashboard(m.snap)
}

func (m *model) unreachableView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("tallowctl") + "\n\n")
	b.WriteString(badStyle.Render("gateway unreachable: "+m.socket) + "\n")
	b.WriteString(mutedStyle.Render("is the gateway running? retrying…\n"))
	b.WriteString(mutedStyle.Render(m.err.Error() + "\n"))
	b.WriteString("\npress q to quit\n")
	return b.String()
}

func renderDashboard(s *snapshot) string {
	var b strings.Builder

	// 1. header
	version := s.health.Version
	if version == "" {
		version = s.config.Version
	}
	title := titleStyle.Render("tallow") + "  " + mutedStyle.Render("v"+version)
	b.WriteString(title + "\n")

	if s.health.Status != "ok" {
		b.WriteString(badStyle.Render("health: "+s.health.Status) + "\n")
	}
	b.WriteString(labelStyle.Render(fmt.Sprintf("uptime %s   config %s",
		fmtUptime(s.metrics.UptimeSeconds), s.config.Path)) + "\n\n")

	// 2. totals
	b.WriteString(sectionStyle.Render("Totals") + "\n")
	to := s.metrics
	fmt.Fprintf(&b, "requests %d   errors %s   cached %s\n",
		to.Total, badOrNum(to.Errors), okOrNum(to.Cached))
	fmt.Fprintf(&b, "tokens %d prompt / %d completion\n",
		to.PromptTokens, to.CompletionTokens)
	fmt.Fprintf(&b, "cost %s   p50 %d ms   p95 %d ms\n",
		dollars(to.CostCents), to.LatencyP50Ms, to.LatencyP95Ms)
	b.WriteString("\n")

	// 3. providers / keys
	b.WriteString(sectionStyle.Render("Providers") + "\n")
	b.WriteString(renderProviders(s.providers))
	b.WriteString("\n")

	// 4. aliases
	b.WriteString(sectionStyle.Render("Aliases") + "\n")
	b.WriteString(renderAliases(s.aliases))
	b.WriteString("\n")

	// 5. recent log
	b.WriteString(sectionStyle.Render("Recent requests") + "\n")
	b.WriteString(renderLogs(s.logs))
	b.WriteString("\n")

	// 6. cache
	c := s.cache
	b.WriteString(sectionStyle.Render("Cache") + "  ")
	fmt.Fprintf(&b, "hit %d   miss %d   evict %d   entries %d\n",
		c.Hit, c.Miss, c.Evict, c.Size)

	return b.String()
}

func badOrNum(n int64) string {
	if n > 0 {
		return badStyle.Render(strconv.FormatInt(n, 10))
	}
	return "0"
}

func okOrNum(n int64) string {
	if n > 0 {
		return okStyle.Render(strconv.FormatInt(n, 10))
	}
	return "0"
}

func healthTag(state string) string {
	color := healthColor(state)
	if state == "" {
		state = "?"
	}
	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(color)).Render(state)
}

func renderProviders(providers []providerView) string {
	if len(providers) == 0 {
		return mutedStyle.Render("  (none)\n")
	}
	var b strings.Builder
	for _, p := range providers {
		fmt.Fprintf(&b, " %s  %s  rpm %s\n", p.Name, healthTag(p.Health), rpmStr(providerRemaining(p)))
		if p.BaseURL != "" {
			b.WriteString(mutedStyle.Render("   " + p.BaseURL + "\n"))
		}
		for _, k := range p.Keys {
			keyName := k.ID
			if keyName == "" {
				keyName = k.Ref
			}
			cost := dollars(k.CostCents)
			if k.CostLimit > 0 {
				cost = fmt.Sprintf("%s/%s", dollars(k.CostCents), dollars(k.CostLimit))
			}
			fmt.Fprintf(&b, "    key %-12s %s  rpm %s  inflight %d/%d  cost %s\n",
				keyName, healthTag(k.Health), rpmStr(k.RPMRemaining),
				k.Inflight, k.MaxInflight, cost)
		}
	}
	return b.String()
}

// providerRemaining aggregates a provider's key-level RPM remaining; when every
// key is unlimited (or there are no keys) it falls back to the configured rate.
func providerRemaining(p providerView) int {
	if len(p.Keys) == 0 {
		return p.RPM
	}
	sum := 0
	allUnlimited := true
	for _, k := range p.Keys {
		if k.RPMRemaining >= 0 {
			sum += k.RPMRemaining
			allUnlimited = false
		}
	}
	if allUnlimited {
		return -1
	}
	return sum
}

func renderAliases(aliases []aliasView) string {
	if len(aliases) == 0 {
		return mutedStyle.Render("  (none)\n")
	}
	var b strings.Builder
	for _, a := range aliases {
		var targets []string
		for _, t := range a.Targets {
			targets = append(targets, fmt.Sprintf("%s(%s)", t.Provider, t.Model))
		}
		fmt.Fprintf(&b, " %s → %s\n", a.Name, strings.Join(targets, ", "))
	}
	return b.String()
}

func renderLogs(logs []logEntry) string {
	if len(logs) == 0 {
		return mutedStyle.Render("  (none)\n")
	}
	if len(logs) > shownLogs {
		logs = logs[:shownLogs]
	}
	var b strings.Builder
	for _, l := range logs {
		status := l.Status
		if status == "" {
			status = "?"
		}
		styled := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(statusColor(status))).Render(status)

		route := l.Provider
		if l.Key != "" {
			route = l.Provider + "/" + keyTail(l.Key)
		}

		tokens := l.PromptTokens + l.CompletionTokens
		model := l.Model
		if model == "" {
			model = l.UpstreamModel
		}

		line := fmt.Sprintf("%-8s %-18s %-14s %6d tok  %6d ms  %s",
			styled, route, model, tokens, l.DurMillis, l.RouteReason)

		if l.Err != "" {
			line += " " + badStyle.Render(l.Err)
		}
		b.WriteString(" " + line + "\n")
	}
	return b.String()
}

// keyTail trims a "/"- or "."-delimited key id to a short tail for the log
// column (long dotted keys stay readable).
func keyTail(key string) string {
	if i := strings.LastIndexByte(key, '/'); i >= 0 && i < len(key)-1 {
		return key[i+1:]
	}
	if i := strings.LastIndexByte(key, '.'); i >= 0 && i < len(key)-1 {
		return key[i+1:]
	}
	return key
}
