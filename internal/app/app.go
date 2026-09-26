// Package app is the composition root: it wires config, secrets, registry,
// routing, budgets, health, cache, store, proxy, and the admin interface into
// a single runnable gateway, and drives hot-reload and proactive health checks.
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/admin"
	"github.com/rishabh-yadav11/tallow/internal/budget"
	"github.com/rishabh-yadav11/tallow/internal/cache"
	"github.com/rishabh-yadav11/tallow/internal/compact"
	"github.com/rishabh-yadav11/tallow/internal/config"
	"github.com/rishabh-yadav11/tallow/internal/health"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/observ"
	"github.com/rishabh-yadav11/tallow/internal/proxy"
	"github.com/rishabh-yadav11/tallow/internal/registry"
	"github.com/rishabh-yadav11/tallow/internal/routing"
	"github.com/rishabh-yadav11/tallow/internal/secret"
	"github.com/rishabh-yadav11/tallow/internal/store"
)

// App is the gateway instance.
type App struct {
	BuildVersion string
	CfgPath      string
	Keystore     *secret.Store
	box          *secret.Box

	mu     sync.RWMutex
	cfg    *config.Config
	params config.Params
	reg    *registry.Registry
	router *routing.Router
	health *health.Registry
	cache  *cache.Cache
	observ *observ.Collector
	store  *store.Store
	client *http.Client

	proxyHandler *proxy.Handler
	adminSrv     *admin.Server
	proxySrv     *http.Server

	authMu sync.RWMutex
	auth   map[string]bool
	// authOpen is the explicit safe-by-default gate: when false (the default),
	// an empty allow-list still rejects unauthenticated requests.
	authOpen bool

	// probeSem bounds concurrent health probes so a large provider/key fanout
	// cannot spawn unbounded goroutines per interval.
	probeSem chan struct{}
}

// New builds and wires the full gateway from a config path.
func New(cfgPath, version string) (*App, error) {
	a := &App{BuildVersion: version, CfgPath: cfgPath, probeSem: make(chan struct{}, 16)}
	if err := a.load(cfgPath); err != nil {
		return nil, err
	}
	return a, nil
}

// load (re)initializes the runtime from the config file at path.
func (a *App) load(path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	params, err := cfg.BuildParams()
	if err != nil {
		return err
	}

	masterKey, err := secret.LoadMasterKey(cfg.Secret.Keyfile)
	if err != nil {
		return err
	}
	box, err := secret.NewBox(masterKey)
	if err != nil {
		return err
	}
	a.box = box
	keystore, err := secret.LoadStore(cfg.Secret.Keystore, box)
	if err != nil {
		return err
	}

	providers, aliases, err := cfg.Build()
	if err != nil {
		return err
	}
	a.resolveSecrets(providers, keystore)

	reg := registry.New(providers, aliases)
	hreg := health.NewRegistry()
	router := routing.NewRouter(reg, hreg, params.StickyTTL, time.Now)
	router.SetBudgets(keyLimits(providers), provRPM(providers))

	st, err := store.Open(store.StoreConfig{
		Path:          cfg.Store.Path,
		RawBodies:     *cfg.Store.RawBodies,
		RawBodiesDays: cfg.Retention.RawBodiesDays,
		MetadataDays:  cfg.Retention.MetadataDays,
		ErrorsDays:    cfg.Retention.ErrorsDays,
		RollupEvery:   params.RollupEvery,
		VacuumEvery:   params.VacuumEvery,
	})
	if err != nil {
		return err
	}
	c := cache.New(cfg.Cache.MaxEntries, params.CacheTTL, maxBytesForCache(cfg))
	obs := observ.New(cfg.Observability.Enabled)
	client := newHTTPClient()

	ph := proxy.NewHandler(proxy.Deps{
		Router:        router,
		Registry:      reg,
		Cache:         c,
		Store:         st,
		Observ:        obs,
		Client:        client,
		Auth:          a.checkAuth,
		MaxConcurrent: cfg.Server.MaxConcurrent,
		QueueTimeout:  params.QueueTimeout,
		Compaction:    compact.Config{Enabled: *cfg.Compaction.Enabled, MaxToolOutputChars: cfg.Compaction.MaxToolOutputChars},
		CacheEnabled:  *cfg.Cache.Enabled,
		RawBodies:     *cfg.Store.RawBodies,
		// H6: the body cap is a server-level concern, so it is resolved once
		// here and enforced by the handler with http.MaxBytesReader.
		MaxRequestBytes: params.MaxBodyBytes,
	})

	a.mu.Lock()
	a.cfg = cfg
	a.params = params
	a.reg = reg
	a.router = router
	a.health = hreg
	a.cache = c
	a.observ = obs
	a.store = st
	a.client = client
	a.Keystore = keystore
	a.proxyHandler = ph
	a.mu.Unlock()

	a.setAuth(cfg.Auth.APIKeys, cfg.Auth.Open)
	a.adminSrv = admin.New(a, cfg.Server.AdminSocket)
	a.proxySrv = newProxyServer(cfg.Server.Listen, ph.Routes(), params)
	return nil
}

// maxHeaderBytes bounds the request header block. It matches net/http's own
// default so nothing changes for well-behaved clients, while making the bound
// explicit and auditable here.
const maxHeaderBytes = 1 << 20

// newProxyServer builds the client-facing HTTP server with explicit timeouts.
//
// H6: the server previously set only ReadTimeout, and only when it happened to
// be positive - which it never was by default, because applyDefaults skipped the
// field. WriteTimeout, IdleTimeout and ReadHeaderTimeout were never set at all,
// so a slowloris client could hold a connection open indefinitely.
//
// ReadHeaderTimeout is set separately from ReadTimeout because it is the one
// that actually bounds header reading; ReadTimeout covers the body, and 0 for
// WriteTimeout is deliberate, because a streaming LLM completion has no useful
// upper bound of its own and the per-provider upstream timeout already governs
// how long a request may take. IdleTimeout bounds the keep-alive pool so idle
// connections do not accumulate.
func newProxyServer(addr string, handler http.Handler, params config.Params) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadTimeout:       params.ReadTimeout,
		ReadHeaderTimeout: readHeaderTimeout(params),
		WriteTimeout:      params.WriteTimeout,
		IdleTimeout:       params.IdleTimeout,
		// Stated explicitly rather than inherited. net/http's default is 1 MiB,
		// which is already a bound, but a limit that is only implicit in another
		// package's default is a limit nobody can audit. 1 MiB is generous for
		// OpenAI-compatible requests, whose bodies are JSON, not file uploads.
		MaxHeaderBytes: maxHeaderBytes,
	}
}

// readHeaderTimeout derives the header-read bound.
//
// The result is ALWAYS positive. A zero ReadHeaderTimeout in net/http means
// "no limit", so returning the caller's value when it is zero would silently
// re-open the slowloris exposure this bound exists to close, and a Config
// assembled without going through applyDefaults (as tests and embedders do)
// would hit exactly that path. The floor therefore applies even to a zero
// read_timeout: a header that cannot be completed quickly is not worth waiting
// for, independent of how long the body is allowed to take.
//
// The cap applies for the same reason in the other direction: a config that
// sets a long read_timeout must not buy an unbounded header read.
func readHeaderTimeout(params config.Params) time.Duration {
	const (
		floor = 10 * time.Second
		cap   = 10 * time.Second
	)
	d := params.ReadTimeout
	if d <= 0 || d < floor {
		return floor
	}
	if d > cap {
		return cap
	}
	return d
}

func (a *App) resolveSecrets(providers []model.Provider, ks *secret.Store) {
	for pi := range providers {
		for ki := range providers[pi].Keys {
			k := &providers[pi].Keys[ki]
			if k.Ref == "" {
				continue
			}
			if s, err := ks.Get(k.Ref); err == nil {
				k.Secret = s
			}
		}
	}
}

// setAuth installs the allow-list and the open flag in ONE critical section.
//
// H5: these were two separate lock acquisitions - setAuth(keys) followed by
// setAuthOpen(open). checkAuth short-circuits on authOpen, so when an operator
// closed an open gateway (open=true -> open=false plus a key list) in a single
// reload there was a window in which the new allow-list was live but authOpen
// was still true, and unauthenticated requests were accepted. The reverse
// direction errs safe: an early open=false rejects requests briefly, which is
// an availability blip rather than a security hole.
//
// Both fields now move together, so no reader can observe one without the other.
func (a *App) setAuth(keys []string, open bool) {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		if k != "" {
			m[k] = true
		}
	}
	a.authMu.Lock()
	a.auth = m
	a.authOpen = open
	a.authMu.Unlock()
}

func (a *App) checkAuth(token string) bool {
	a.authMu.RLock()
	defer a.authMu.RUnlock()
	if a.authOpen {
		return true
	}
	return a.auth[token]
}

// maxBytesForCache returns the configured cache byte bound, falling back to a
// 256 MiB default when the config did not set a positive value.
func maxBytesForCache(cfg *config.Config) int64 {
	if cfg.Cache.MaxBytes > 0 {
		return cfg.Cache.MaxBytes
	}
	return 256 * 1024 * 1024
}

func keyLimits(providers []model.Provider) map[string]budget.KeyLimits {
	m := map[string]budget.KeyLimits{}
	for _, p := range providers {
		for _, k := range p.Keys {
			m[p.Name+"/"+k.ID] = budget.KeyLimits{
				RPM:             k.RPM,
				MaxRequests:     k.MaxRequests,
				Window:          k.Window,
				MaxConcurrent:   k.MaxConcurrent,
				CostLimitCents:  k.CostLimitCents,
				CostLimitMicros: k.CostLimitMicros,
			}
		}
	}
	return m
}

func provRPM(providers []model.Provider) map[string]int {
	m := map[string]int{}
	for _, p := range providers {
		if p.RPM > 0 {
			m[p.Name] = p.RPM
		}
	}
	return m
}

func newHTTPClient() *http.Client {
	// M14: the dialer is where the SSRF policy is enforced, because the resolved
	// address - not the base_url string - is what decides whether a request
	// carrying the provider Authorization header and the full user prompt is
	// about to leave the machine for a host inside the network. Control runs
	// before the socket is created, so a refused connection never reaches the
	// internal host, and running per-dial rather than per-request also covers a
	// pooled idle connection and a name that re-resolves between requests.
	//
	// The dialer is set as a field rather than via Transport.DialContext so that
	// it applies uniformly, and Proxy is honoured, which means a deployment that
	// sets HTTPS_PROXY still gets the guard on the proxy hop.
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   config.DialControl,
	}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		Proxy:                 http.ProxyFromEnvironment,
	}
	return &http.Client{Transport: tr, CheckRedirect: refuseCredentialRedirect}
}

// refuseCredentialRedirect refuses to follow a redirect on the upstream client.
//
// M2: with Go's default CheckRedirect, a 307 or 308 from an upstream causes the
// client to re-send the request - including the `Authorization: Bearer
// <provider secret>` header - to whatever host the redirect names. A
// compromised, misconfigured, or attacker-controlled upstream could therefore
// harvest provider credentials by redirecting to a host it controls, and Go
// preserves headers on the same-host case too, so even a "same host only" rule
// would leak on a host that resolves to an attacker address.
//
// The guarantee enforced here is absolute and simple: the upstream request goes
// to exactly the URL the router selected, and nowhere else. Redirects are
// refused rather than followed-and-sanitised because every follow is a chance
// to leak, and a legitimate OpenAI-compatible endpoint does not redirect a
// POSTed chat completion.
//
// ErrUseLastResponse makes the client return the 3xx response as-is rather than
// treating it as a failure, so the caller sees the redirect status and produces
// a normal retryable-or-not decision from it. Returning a bare error would
// surface as a transport error and lose the status.
func refuseCredentialRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// ProxyHandler exposes the client-facing HTTP handler.
func (a *App) ProxyHandler() http.Handler { return a.proxyHandler.Routes() }

// ListenAddr returns the client-facing HTTP listen address.
func (a *App) ListenAddr() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.Server.Listen
}

// AdminSocket returns the admin control-socket path.
func (a *App) AdminSocket() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.Server.AdminSocket
}

// publishHook is a test-only seam invoked inside Reload after budgets are
// published but before the registry snapshot is swapped. It exists because the
// audit C4 invariant is a property of the ORDER of two independently-locked
// mutations, and the window between them is far too narrow to observe from a
// concurrent test. It is nil in production, so the call is a single nil check
// on a package-level var and changes nothing about reload behavior.
//
// Do not use it to mutate state; only to assert.
var publishHook func()

// Reload re-reads config and swaps the live state (hot reload). It reuses all
// long-lived objects (store, cache, collector, HTTP client, proxy handler) so
// no handles/connections leak and no counters are reset on reload.
func (a *App) Reload() error {
	cfg, err := config.Load(a.CfgPath)
	if err != nil {
		return err
	}
	params, err := cfg.BuildParams()
	if err != nil {
		return err
	}
	keystore, err := secret.LoadStore(cfg.Secret.Keystore, a.box)
	if err != nil {
		return err
	}
	providers, aliases, err := cfg.Build()
	if err != nil {
		return err
	}
	a.resolveSecrets(providers, keystore)

	a.mu.Lock()
	a.cfg = cfg
	a.params = params
	// C4: budgets are published BEFORE the registry, deliberately.
	//
	// The invariant that matters is "every routable key has a budget entry", and
	// the two halves are mutated under two different locks (r.bmu and reg.mu),
	// so no single critical section can hold it. Ordering alone can:
	//
	//   - Budgets first: a key that is not yet in the registry is simply not
	//     routable, so nothing can reach it. The stray budget entry is inert and
	//     is swept by the next SetBudgets. There is no window in which a
	//     routable key lacks a budget.
	//   - Registry first (the old order): a newly added key IS routable the
	//     instant Swap returns, and its budget arrives two statements later.
	//     Every request in that window reads kb == nil. The router now rejects
	//     those (errNoBudget) rather than treating them as unlimited, so the
	//     window fails closed instead of open - but it would still reject
	//     traffic to a perfectly valid key, which is an outage, not a
	//     correctness fix. Publishing budgets first removes the window itself.
	//
	// Removal is safe under this order for the same reason: Swap drops the
	// provider first, so an orphaned budget is briefly unreachable but never
	// unrestrained.
	a.router.SetBudgets(keyLimits(providers), provRPM(providers))
	if publishHook != nil {
		// Test-only seam. The C4 invariant is a property of the ORDER of two
		// mutations, and the window between them is nanoseconds wide, so a
		// concurrent observer cannot reliably catch a regression. This hook
		// lets a test inspect the intermediate state deterministically. It is
		// nil in production; see c4_publish_test.go.
		publishHook()
	}
	a.reg.Swap(providers, aliases)
	a.health.Prune(providers)
	a.observ.SetEnabled(cfg.Observability.Enabled)
	a.Keystore = keystore
	a.mu.Unlock()

	a.setAuth(cfg.Auth.APIKeys, cfg.Auth.Open)
	if a.store != nil {
		_ = a.store.Audit("reload", "config", a.CfgPath)
	}
	return nil
}

// Run starts all servers and background workers, blocking until ctx cancels.
func (a *App) Run(ctx context.Context) error {
	errc := make(chan error, 4)

	go func() { errc <- a.store.RunRetention(ctx) }()
	go func() { errc <- a.healthLoop(ctx) }()
	go func() { errc <- a.watchConfig(ctx) }()
	go func() { errc <- a.adminSrv.Serve(ctx) }()
	go func() {
		if err := a.proxySrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		a.shutdown()
		return err
	case <-ctx.Done():
		a.shutdown()
		return nil
	}
}

// shutdown gracefully stops the proxy and admin servers within a bounded deadline
// so a lingering keep-alive client connection cannot block process shutdown
// indefinitely.
func (a *App) shutdown() {
	const grace = 10 * time.Second
	sctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	_ = a.proxySrv.Shutdown(sctx)
	a.adminSrv.Close()
	a.store.Close()
}

// watchConfig polls the config file mtime and hot-reloads on change.
func (a *App) watchConfig(ctx context.Context) error {
	var last time.Time
	if st, err := os.Stat(a.CfgPath); err == nil {
		last = st.ModTime()
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			st, err := os.Stat(a.CfgPath)
			if err != nil {
				continue
			}
			if st.ModTime().After(last) {
				last = st.ModTime()
				if err := a.Reload(); err != nil {
					fmt.Fprintf(os.Stderr, "tallow: reload failed: %v\n", err)
				}
			}
		}
	}
}

// healthLoop proactively probes providers/keys, folding into circuit breakers.
func (a *App) healthLoop(ctx context.Context) error {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	last := map[string]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-t.C:
			a.mu.RLock()
			reg := a.reg
			client := a.client
			aliasTargets := a.aliasModelByProvider()
			a.mu.RUnlock()
			for _, p := range reg.Providers() {
				if !p.HealthCheck {
					continue
				}
				interval := p.HealthInterval
				if interval <= 0 {
					interval = 60 * time.Second
				}
				if now.Sub(last[p.Name]) < interval {
					continue
				}
				last[p.Name] = now
				model := aliasTargets[p.Name]
				if model == "" {
					continue
				}
				for _, k := range p.Keys {
					select {
					case a.probeSem <- struct{}{}:
						go func() {
							defer func() { <-a.probeSem }()
							a.probe(reg, client, p, k, model)
						}()
					default:
						// too many probes in flight; skip this round for this key
					}
				}
			}
		}
	}
}

// probe sends a minimal 1-token chat completion to validate a key/provider.
func (a *App) probe(reg *registry.Registry, client *http.Client, p model.Provider, k model.Key, model string) {
	body := map[string]any{
		"model": model, "max_tokens": 1, "stream": false,
		"messages": []map[string]string{{"role": "user", "content": "ping"}},
	}
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, trimSlash(p.BaseURL)+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+k.Secret)
	resp, err := client.Do(req)
	ok := err == nil && resp != nil && resp.StatusCode < 400
	if resp != nil {
		resp.Body.Close()
	}
	now := time.Now()
	pb := a.health.Provider(p.Name)
	kb := a.health.Key(p.Name, k.ID)
	if ok {
		pb.RecordSuccess(now)
		kb.RecordSuccess(now)
	} else {
		pb.RecordFailure(now)
		kb.RecordFailure(now)
	}
}

func (a *App) aliasModelByProvider() map[string]string {
	aliases := a.reg.Aliases()
	// Iterate in a stable order so "first-seen" model per provider is
	// deterministic (reg.aliases is a map with random iteration order), which
	// both keeps the health probe stable and makes behavior testable.
	sort.Slice(aliases, func(i, j int) bool { return aliases[i].Name < aliases[j].Name })
	m := map[string]string{}
	for _, al := range aliases {
		for _, t := range al.Targets {
			// Pick a model from a target whose Provider matches this key (the
			// provider being probed), i.e. a model this provider actually
			// serves, rather than blindly reusing the first alias's target.
			// If none is recorded yet, fall back to the first target's model.
			if _, ok := m[t.Provider]; !ok {
				m[t.Provider] = t.Model
			}
		}
	}
	return m
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// ---------- admin.Backend implementation ----------

// Metrics returns the live observability snapshot.
func (a *App) Metrics() observ.Snapshot { return a.observ.Snapshot() }

// ProvidersView returns providers + keys with live health and budgets.
func (a *App) ProvidersView() []admin.ProviderView {
	now := time.Now()
	var out []admin.ProviderView
	for _, p := range a.reg.Providers() {
		pv := admin.ProviderView{
			Name:    p.Name,
			BaseURL: p.BaseURL,
			RPM:     p.RPM,
			Health:  a.health.Provider(p.Name).State(now).String(),
		}
		for _, k := range p.Keys {
			s := keyBudgetSnapshot(a.router, p.Name, k.ID, now)
			pv.Keys = append(pv.Keys, admin.KeyView{
				ID:           k.ID,
				Ref:          k.Ref,
				RPM:          k.RPM,
				Health:       a.health.Key(p.Name, k.ID).State(now).String(),
				RPMRemaining: s.RPMRemaining,
				Inflight:     s.Inflight,
				MaxInflight:  s.MaxInflight,
				CostCents:    s.CostCents,
				CostLimit:    s.CostLimit,
			})
		}
		out = append(out, pv)
	}
	return out
}

// AliasesView returns aliases + targets.
func (a *App) AliasesView() []admin.AliasView {
	var out []admin.AliasView
	for _, al := range a.reg.Aliases() {
		av := admin.AliasView{Name: al.Name}
		for _, t := range al.Targets {
			av.Targets = append(av.Targets, admin.TargetView{Provider: t.Provider, Model: t.Model})
		}
		out = append(out, av)
	}
	return out
}

// BudgetsView returns live per-key budget status.
func (a *App) BudgetsView() []routing.BudgetStatus { return a.router.Budgets(time.Now()) }

// HealthView returns provider/key breaker states.
func (a *App) HealthView() map[string]string {
	now := time.Now()
	m := map[string]string{}
	for _, p := range a.reg.Providers() {
		m["provider:"+p.Name] = a.health.Provider(p.Name).State(now).String()
		for _, k := range p.Keys {
			m["key:"+p.Name+"/"+k.ID] = a.health.Key(p.Name, k.ID).State(now).String()
		}
	}
	return m
}

// RecentRequests returns the latest request metadata from the store.
func (a *App) RecentRequests(n int) []model.RequestMeta {
	r, _ := a.store.RecentRequests(n)
	return r
}

// RecentRollups returns the latest cost/usage rollups.
func (a *App) RecentRollups(n int) []store.Rollup {
	r, _ := a.store.RecentRollups(n)
	return r
}

// RecentAudit returns the audit log tail.
func (a *App) RecentAudit(n int) []store.AuditEntry {
	r, _ := a.store.RecentAudit(n)
	return r
}

// CacheStats returns cache hit/miss/evict/size.
func (a *App) CacheStats() (int64, int64, int64, int64) { return a.cache.Stats() }

// Version returns the build version.
func (a *App) Version() string { return a.BuildVersion }

// ConfigPath returns the loaded config path.
func (a *App) ConfigPath() string { return a.CfgPath }

func keyBudgetSnapshot(r *routing.Router, provider, key string, now time.Time) routing.BudgetStatus {
	for _, b := range r.Budgets(now) {
		if b.Provider == provider && b.Key == key {
			return b
		}
	}
	return routing.BudgetStatus{}
}
