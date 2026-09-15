// Package app is the composition root: it wires config, secrets, registry,
// routing, budgets, health, cache, store, proxy, and the admin interface into
// a single runnable gateway, and drives hot-reload and proactive health checks.
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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

	a.setAuth(cfg.Auth.APIKeys)
	a.setAuthOpen(cfg.Auth.Open)
	a.adminSrv = admin.New(a, cfg.Server.AdminSocket)
	a.proxySrv = &http.Server{Addr: cfg.Server.Listen, Handler: ph.Routes()}
	if params.ReadTimeout > 0 {
		a.proxySrv.ReadTimeout = params.ReadTimeout
	}
	return nil
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

func (a *App) setAuth(keys []string) {
	m := map[string]bool{}
	for _, k := range keys {
		if k != "" {
			m[k] = true
		}
	}
	a.authMu.Lock()
	a.auth = m
	a.authMu.Unlock()
}

// setAuthOpen records the safe-by-default open flag. Callers must hold authMu.
func (a *App) setAuthOpen(open bool) {
	a.authMu.Lock()
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
				RPM:            k.RPM,
				MaxRequests:    k.MaxRequests,
				Window:         k.Window,
				MaxConcurrent:  k.MaxConcurrent,
				CostLimitCents: k.CostLimitCents,
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
	tr := &http.Transport{
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		Proxy:                 http.ProxyFromEnvironment,
	}
	return &http.Client{Transport: tr}
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
	a.reg.Swap(providers, aliases)
	a.router.SetBudgets(keyLimits(providers), provRPM(providers))
	a.observ.SetEnabled(cfg.Observability.Enabled)
	a.Keystore = keystore
	a.mu.Unlock()

	a.setAuth(cfg.Auth.APIKeys)
	a.setAuthOpen(cfg.Auth.Open)
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
	m := map[string]string{}
	for _, al := range a.reg.Aliases() {
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
