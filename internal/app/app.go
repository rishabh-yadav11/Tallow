package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/admin"
	"github.com/rishabh-yadav11/tallow/internal/budget"
	"github.com/rishabh-yadav11/tallow/internal/cache"
	"github.com/rishabh-yadav11/tallow/internal/catalog"
	"github.com/rishabh-yadav11/tallow/internal/compact"
	"github.com/rishabh-yadav11/tallow/internal/config"
	"github.com/rishabh-yadav11/tallow/internal/health"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/observ"
	"github.com/rishabh-yadav11/tallow/internal/proxy"
	"github.com/rishabh-yadav11/tallow/internal/registry"
	"github.com/rishabh-yadav11/tallow/internal/resilience"
	"github.com/rishabh-yadav11/tallow/internal/routing"
	"github.com/rishabh-yadav11/tallow/internal/secret"
	"github.com/rishabh-yadav11/tallow/internal/store"
)

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

	resilience *resilience.Manager

	catalogFetchers map[string]*catalog.Fetcher

	authMu sync.RWMutex
	auth   map[string]bool

	authOpen bool

	probeSem chan struct{}
}

func New(cfgPath, version string) (*App, error) {
	a := &App{BuildVersion: version, CfgPath: cfgPath, probeSem: make(chan struct{}, 16)}
	if err := a.load(cfgPath); err != nil {
		return nil, err
	}
	return a, nil
}

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

	mgr := resilience.NewManager(resilience.RetryPolicy{
		RequestRetryDefault: params.RequestRetry,
		MaxRetryCredentials: params.MaxRetryCredentials,
		MaxRetryInterval:    params.MaxRetryInterval,
	}, cfg.Routing.CooldownFile, nil)
	if err := mgr.Load(); err != nil {
		slog.Error("cooldown load failed; continuing with in-memory state",
			"path", cfg.Routing.CooldownFile, "err", err)
	}
	router.SetExcluder(mgr.Excludes())

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

	fetchers := map[string]*catalog.Fetcher{}
	for _, p := range providers {
		if !p.Catalog {
			continue
		}
		fetchers[p.Name] = catalog.NewFetcher(p.Name, p.BaseURL, client, true)
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

		MaxRequestBytes: params.MaxBodyBytes,
		Keepalive:       params.StreamKeepalive,
		Resilience:      mgr,
		ModelsExtra:     a.catalogModels,
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
	a.resilience = mgr
	a.catalogFetchers = fetchers
	a.mu.Unlock()

	a.setAuth(cfg.Auth.APIKeys, cfg.Auth.Open)
	a.adminSrv = admin.New(a, cfg.Server.AdminSocket)
	a.proxySrv = newProxyServer(cfg.Server.Listen, ph.Routes(), params)
	return nil
}

const maxHeaderBytes = 1 << 20

func newProxyServer(addr string, handler http.Handler, params config.Params) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadTimeout:       params.ReadTimeout,
		ReadHeaderTimeout: readHeaderTimeout(params),
		WriteTimeout:      params.WriteTimeout,
		IdleTimeout:       params.IdleTimeout,

		MaxHeaderBytes: maxHeaderBytes,
	}
}

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

				slog.Error("keystore entry unavailable; key secret left empty",
					"provider", providers[pi].Name,
					"key", k.ID,
					"ref", k.Ref,
					"err", err)
			}
		}
	}
}

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

func refuseCredentialRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func (a *App) ProxyHandler() http.Handler { return a.proxyHandler.Routes() }

func (a *App) ListenAddr() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.Server.Listen
}

func (a *App) AdminSocket() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.Server.AdminSocket
}

var publishHook func()

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

	a.router.SetBudgets(keyLimits(providers), provRPM(providers))
	if publishHook != nil {

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

const cooldownPersistInterval = 30 * time.Second

func (a *App) Run(ctx context.Context) error {
	errc := make(chan error, 4)

	go func() { errc <- a.store.RunRetention(ctx) }()
	go func() { errc <- a.healthLoop(ctx) }()
	go func() { errc <- a.watchConfig(ctx) }()
	go func() { errc <- a.adminSrv.Serve(ctx) }()
	go func() {
		var err error
		a.mu.RLock()
		cert, key := a.cfg.Server.TLSCert, a.cfg.Server.TLSKey
		a.mu.RUnlock()
		if cert != "" && key != "" {

			err = a.proxySrv.ListenAndServeTLS(cert, key)
		} else {
			err = a.proxySrv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()

	go a.persistCooldowns(ctx)

	select {
	case err := <-errc:
		a.shutdown()
		return err
	case <-ctx.Done():
		a.shutdown()
		return nil
	}
}

func (a *App) persistCooldowns(ctx context.Context) {
	t := time.NewTicker(cooldownPersistInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.mu.RLock()
			mgr := a.resilience
			a.mu.RUnlock()
			if mgr == nil {
				continue
			}
			if err := mgr.Persist(); err != nil {
				slog.Error("cooldown persist failed", "err", err)
			}
		}
	}
}

func (a *App) shutdown() {
	const grace = 10 * time.Second
	sctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	_ = a.proxySrv.Shutdown(sctx)
	a.adminSrv.Close()
	a.store.Close()
}

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

var healthTick = 15 * time.Second

func (a *App) healthLoop(ctx context.Context) error {
	t := time.NewTicker(healthTick)
	defer t.Stop()
	last := map[string]time.Time{}
	var lastCatalogRefresh time.Time
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-t.C:
			a.refreshCatalogs(now, &lastCatalogRefresh)
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

					}
				}
			}
		}
	}
}




const upstreamResponsesPath = "/responses"



const catalogRefreshInterval = 5 * time.Minute

func (a *App) refreshCatalogs(now time.Time, last *time.Time) {
	if now.Sub(*last) < catalogRefreshInterval {
		return
	}
	*last = now
	a.mu.RLock()
	fetchers := make(map[string]*catalog.Fetcher, len(a.catalogFetchers))
	for name, f := range a.catalogFetchers {
		fetchers[name] = f
	}
	a.mu.RUnlock()
	if len(fetchers) == 0 {
		return
	}
	names := make([]string, 0, len(fetchers))
	for name := range fetchers {
		names = append(names, name)
	}
	sort.Strings(names)
	go func() {
		for _, name := range names {
			if err := fetchers[name].Refresh(); err != nil {

				slog.Debug("catalog refresh failed", "provider", name, "err", err)
			}
		}
	}()
}

func (a *App) catalogModels() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.catalogFetchers) == 0 {
		return nil
	}
	manual := map[string]bool{}
	for _, al := range a.reg.Aliases() {
		for _, t := range al.Targets {
			manual[t.Provider] = true
		}
	}
	names := make([]string, 0, len(a.catalogFetchers))
	for name := range a.catalogFetchers {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []string
	for _, name := range names {
		if manual[name] {
			continue
		}
		out = append(out, a.catalogFetchers[name].Models()...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (a *App) probe(reg *registry.Registry, client *http.Client, p model.Provider, k model.Key, model string) {
	body := map[string]any{
		"model":             model,
		"input":             "ping",
		"max_output_tokens": 1,
		"stream":            false,
	}
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, trimSlash(p.BaseURL)+upstreamResponsesPath, bytes.NewReader(b))
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

	sort.Slice(aliases, func(i, j int) bool { return aliases[i].Name < aliases[j].Name })
	m := map[string]string{}
	for _, al := range aliases {
		for _, t := range al.Targets {

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

func (a *App) Metrics() observ.Snapshot { return a.observ.Snapshot() }

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

func (a *App) BudgetsView() []routing.BudgetStatus { return a.router.Budgets(time.Now()) }

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

func (a *App) RecentRequests(n int) []model.RequestMeta {
	r, _ := a.store.RecentRequests(n)
	return r
}

func (a *App) RecentRollups(n int) []store.Rollup {
	r, _ := a.store.RecentRollups(n)
	return r
}

func (a *App) RecentAudit(n int) []store.AuditEntry {
	r, _ := a.store.RecentAudit(n)
	return r
}

func (a *App) CacheStats() (int64, int64, int64, int64) { return a.cache.Stats() }

func (a *App) Version() string { return a.BuildVersion }

func (a *App) ConfigPath() string { return a.CfgPath }

func keyBudgetSnapshot(r *routing.Router, provider, key string, now time.Time) routing.BudgetStatus {
	for _, b := range r.Budgets(now) {
		if b.Provider == provider && b.Key == key {
			return b
		}
	}
	return routing.BudgetStatus{}
}
