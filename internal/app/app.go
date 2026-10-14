// Package app is the composition root: it wires config, secrets, registry,
// routing, budgets, health, cache, store, proxy, and the admin interface into
// a single runnable gateway, and drives hot-reload and proactive health checks.
package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

	"github.com/BurntSushi/toml"
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
		RawBodiesDays: *cfg.Retention.RawBodiesDays,
		MetadataDays:  *cfg.Retention.MetadataDays,
		ErrorsDays:    *cfg.Retention.ErrorsDays,
		RollupEvery:   params.RollupEvery,
		VacuumEvery:   params.VacuumEvery,
	})
	if err != nil {
		return err
	}
	c := cache.New(cfg.Cache.MaxEntries, params.CacheTTL, maxBytesForCache(cfg))
	obs := observ.New(cfg.Observability.Enabled)
	client := newHTTPClient()

	sk, _ := config.Dur(cfg.Server.StreamKeepalive)
	tlsCfg, err := serverTLS(cfg.Server)
	if err != nil {
		return err
	}

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
		StreamKeepalive: sk,
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
	a.proxySrv = newProxyServer(cfg.Server.Listen, ph.Routes(), params, tlsCfg)
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
func newProxyServer(addr string, handler http.Handler, params config.Params, tlsCfg *tls.Config) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadTimeout:       params.ReadTimeout,
		ReadHeaderTimeout: readHeaderTimeout(params),
		WriteTimeout:      params.WriteTimeout,
		IdleTimeout:       params.IdleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		TLSConfig:         tlsCfg,
	}
}

// loadTLSConfig builds the HTTPS server config from the configured cert and
// key file. It fails fast (before any socket is bound) on a partially-declared
// TLS, or when the cert cannot be loaded.
func loadTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load tls cert/key: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
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
			} else {
				// A missing/unreadable entry leaves the secret empty; surface it
				// at config load instead of a silent runtime 401, but don't
				// abort the whole reload for one bad key.
				slog.Error("keystore entry unavailable; key secret left empty",
					"provider", providers[pi].Name,
					"key", k.ID,
					"ref", k.Ref,
					"err", err)
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

// serverTLS returns the HTTPS listener config when TLS is enabled, or nil when
// neither tls_cert nor tls_key is set. Setting exactly one is a config error:
// the listener would otherwise promise TLS it cannot serve.
func serverTLS(s config.Server) (*tls.Config, error) {
	cert, key := s.TLSCert, s.TLSKey
	if cert == "" && key == "" {
		return nil, nil
	}
	if cert == "" || key == "" {
		return nil, fmt.Errorf("tls requires both server.tls_cert and server.tls_key; got only one")
	}
	return loadTLSConfig(cert, key)
}

// Run starts all servers and background workers, blocking until ctx cancels.
func (a *App) Run(ctx context.Context) error {
	errc := make(chan error, 4)

	go func() { errc <- a.store.RunRetention(ctx) }()
	go func() { errc <- a.healthLoop(ctx) }()
	go func() { errc <- a.watchConfig(ctx) }()
	go func() { errc <- a.adminSrv.Serve(ctx) }()
	go func() {
		err := a.serveProxy()
		if err != nil && err != http.ErrServerClosed {
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

// serveProxy starts the client-facing listener, TLS when configured, plain
// HTTP otherwise. The server's TLSConfig is set at build time in newProxyServer.
func (a *App) serveProxy() error {
	// TLSConfig set means TLS was configured and the cert already loaded; pass
	// empty paths so net/http uses the server's own TLSConfig rather than
	// re-reading the files (which would also defeat the fail-fast startup load).
	if a.proxySrv.TLSConfig != nil {
		return a.proxySrv.ListenAndServeTLS("", "")
	}
	return a.proxySrv.ListenAndServe()
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
				// Deleted/unreadable config file: log so the operator knows the
				// watcher is stuck on stale state rather than silently ignoring.
				slog.Error("config watcher: stat config failed",
					"path", a.CfgPath, "err", err)
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

// healthTick is how often healthLoop wakes up to consider probing. It is a var
// rather than a constant so tests can drive the loop's scheduling without
// waiting out a 15s tick, which is the same test-only-seam approach publishHook
// uses below. It is 15s in production and nothing writes to it.
var healthTick = 15 * time.Second

// healthLoop proactively probes providers/keys, folding into circuit breakers.
func (a *App) healthLoop(ctx context.Context) error {
	t := time.NewTicker(healthTick)
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

// configMap renders the live Config as a generic JSON-shaped map. The struct is
// serialized through a TOML round-trip so the TOML field names survive and every
// knob is navigable by name; the map form is what both ConfigRead (read) and
// ConfigWrite (mutate) operate on generically. Pointers are dereferenced by the
// round-trip, which is fine: defaults re-apply on the next Load.
func (a *App) configMap() (map[string]any, error) {
	a.mu.RLock()
	cfg := a.cfg
	a.mu.RUnlock()
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return nil, fmt.Errorf("config: serialize: %w", err)
	}
	var m map[string]any
	if _, err := tomlUnmarshal(buf.String(), &m); err != nil {
		return nil, fmt.Errorf("config: deserialize: %w", err)
	}
	return m, nil
}

// ConfigRead returns the live config at scope as JSON: nil scope = the full
// document, ["server"] = the server section, ["server","listen"] = one field.
func (a *App) ConfigRead(scope []string) (json.RawMessage, error) {
	m, err := a.configMap()
	if err != nil {
		return nil, err
	}
	v, ok := configNavigate(m, scope)
	if !ok {
		return nil, admin.ErrConfigNotFound
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("config: marshal: %w", err)
	}
	return json.RawMessage(b), nil
}

// ConfigWrite persists and hot-reloads a config mutation (PUT replace, PATCH
// merge, DELETE remove) at scope. It validates before touching runtime and
// refuses invalid changes without altering the running gateway.
func (a *App) ConfigWrite(method string, scope []string, body json.RawMessage) error {
	if method == http.MethodDelete {
		if len(scope) == 0 {
			return errors.New("cannot delete the whole config document")
		}
		return a.mutateConfig(func(m map[string]any) error { return configDeletePath(m, scope) })
	}
	if !json.Valid(body) {
		return errors.New("config: request body is not valid JSON")
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return fmt.Errorf("config: decode body: %w", err)
	}
	return a.mutateConfig(func(m map[string]any) error {
		return configApplyPath(m, method, scope, value)
	})
}

// mutateConfig applies a mutation to the live config, persists the result to
// the on-disk file, validates it via config.Load, and only swaps the runtime
// (via Reload) if it is valid. An invalid change is refused and never touches
// the live file.
func (a *App) mutateConfig(mut func(map[string]any) error) error {
	m, err := a.configMap()
	if err != nil {
		return err
	}
	if err := mut(m); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return fmt.Errorf("config: serialize change: %w", err)
	}

	// Write to a temp file in the same directory, run config.Load on it (Load
	// parses + applies defaults + validates), and only atomically rename it into
	// place once it is valid. This keeps an invalid write from ever corrupting
	// the live file or, on reload failure, from being half-applied.
	dir := filepath.Dir(a.CfgPath)
	tmp, err := os.CreateTemp(dir, ".tallow-config-*.toml")
	if err != nil {
		return fmt.Errorf("config: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return fmt.Errorf("config: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: close temp: %w", err)
	}
	if _, err := config.Load(tmpName); err != nil {
		return fmt.Errorf("config: validation failed, change refused: %w", err)
	}
	if err := os.Rename(tmpName, a.CfgPath); err != nil {
		return fmt.Errorf("config: replace file: %w", err)
	}
	if err := a.Reload(); err != nil {
		return fmt.Errorf("config: hot-reload failed: %w", err)
	}
	return nil
}

// ReplayRequest re-runs a stored request through the current proxy pipeline and
// writes the response to w.
func (a *App) ReplayRequest(w http.ResponseWriter, id string) error {
	a.mu.RLock()
	st := a.store
	ph := a.proxyHandler
	a.mu.RUnlock()
	if st == nil {
		return errors.New("replay: store unavailable")
	}
	if ph == nil {
		return errors.New("replay: proxy handler unavailable")
	}
	rec, err := st.RawBody(id)
	if err != nil {
		return err
	}
	// Only the chat-completions route records raw bodies, so that is the route
	// path a replay is re-dispatched through; the body is the stored original.
	req, err := http.NewRequest(http.MethodPost, "http://internal/replay", bytes.NewReader(rec.ReqBody))
	if err != nil {
		return fmt.Errorf("replay: construct request: %w", err)
	}
	req.URL.Path = "/v1/chat/completions"
	req.Header.Set("Content-Type", "application/json")
	ph.Routes().ServeHTTP(w, req)
	return nil
}

// KeyRefs returns the keystore refs (never plaintext).
func (a *App) KeyRefs() []string {
	if a.Keystore == nil {
		return nil
	}
	return a.Keystore.List()
}

// UploadKey encrypts and persists a raw secret under ref.
func (a *App) UploadKey(ref string, plaintext []byte) error {
	if a.Keystore == nil {
		return errors.New("keystore unavailable")
	}
	if err := a.Keystore.Set(ref, string(plaintext)); err != nil {
		return fmt.Errorf("upload key: %w", err)
	}
	return nil
}

// DeleteKey removes a credential by ref.
func (a *App) DeleteKey(ref string) error {
	if a.Keystore == nil {
		return errors.New("keystore unavailable")
	}
	if err := a.Keystore.Delete(ref); err != nil {
		return fmt.Errorf("delete key: %w", err)
	}
	return nil
}

// configNavigate walks scope through a JSON-shaped map, returning the value and
// whether the path exists.
func configNavigate(m map[string]any, scope []string) (any, bool) {
	cur := any(m)
	for _, key := range scope {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// configApplyPath applies a PUT (replace) or PATCH (merge) value at scope.
func configApplyPath(m map[string]any, method string, scope []string, value any) error {
	obj, isObj := value.(map[string]any)
	if len(scope) == 0 {
		if !isObj {
			return errors.New("full-document config must be a JSON object")
		}
		if method == "PUT" {
			for k := range m {
				delete(m, k)
			}
		}
		deepMergeConfig(m, obj)
		return nil
	}
	parent := m
	for _, key := range scope[:len(scope)-1] {
		pm, ok := parent[key].(map[string]any)
		if !ok {
			return fmt.Errorf("config: section %q does not exist", key)
		}
		parent = pm
	}
	last := scope[len(scope)-1]
	if method == "PATCH" {
		if cur, ok := parent[last].(map[string]any); ok && isObj {
			deepMergeConfig(cur, obj)
			return nil
		}
	}
	parent[last] = value
	return nil
}

// deepMergeConfig recursively merges src into dst, overwriting scalars and
// recursing into nested objects.
func deepMergeConfig(dst, src map[string]any) {
	for k, v := range src {
		sm, ok := v.(map[string]any)
		if !ok {
			dst[k] = v
			continue
		}
		if dm, ok2 := dst[k].(map[string]any); ok2 {
			deepMergeConfig(dm, sm)
			continue
		}
		dst[k] = v
	}
}

// configDeletePath removes the value at scope.
func configDeletePath(m map[string]any, scope []string) error {
	if len(scope) == 0 {
		return errors.New("cannot delete the whole config document")
	}
	parent := m
	for _, key := range scope[:len(scope)-1] {
		pm, ok := parent[key].(map[string]any)
		if !ok {
			return fmt.Errorf("config: section %q does not exist", key)
		}
		parent = pm
	}
	delete(parent, scope[len(scope)-1])
	return nil
}

func tomlUnmarshal(text string, v any) (toml.MetaData, error) {
	return toml.Decode(text, v)
}

func keyBudgetSnapshot(r *routing.Router, provider, key string, now time.Time) routing.BudgetStatus {
	for _, b := range r.Budgets(now) {
		if b.Provider == provider && b.Key == key {
			return b
		}
	}
	return routing.BudgetStatus{}
}
