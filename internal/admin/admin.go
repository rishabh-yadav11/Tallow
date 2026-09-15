// Package admin is the stable, minimal control interface. It exposes gateway
// state and controls (metrics, config, live budgets, logs) over HTTP bound to
// a local Unix domain socket. Frontends (the TUI, or any future web UI) talk to
// this interface and never reach into gateway internals.
package admin

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/observ"
	"github.com/rishabh-yadav11/tallow/internal/routing"
	"github.com/rishabh-yadav11/tallow/internal/store"
)

// Backend is the read/control surface the gateway core exposes. The app
// implements it; the admin server and any frontend consume only this.
type Backend interface {
	Metrics() observ.Snapshot
	ProvidersView() []ProviderView
	AliasesView() []AliasView
	BudgetsView() []routing.BudgetStatus
	HealthView() map[string]string
	RecentRequests(n int) []model.RequestMeta
	RecentRollups(n int) []store.Rollup
	RecentAudit(n int) []store.AuditEntry
	CacheStats() (hit, miss, evict, size int64)
	Reload() error
	Version() string
	ConfigPath() string
}

// ProviderView is the admin/tool view of a provider + its keys.
type ProviderView struct {
	Name    string    `json:"name"`
	BaseURL string    `json:"base_url"`
	RPM     int       `json:"rpm"`
	Health  string    `json:"health"`
	Keys    []KeyView `json:"keys"`
}

// KeyView is a key's live status (secret never exposed).
type KeyView struct {
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

// AliasView is a client-facing alias and its targets.
type AliasView struct {
	Name    string       `json:"name"`
	Targets []TargetView `json:"targets"`
}

// TargetView is one provider target of an alias.
type TargetView struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Server is the admin HTTP server bound to a Unix socket.
type Server struct {
	b    Backend
	path string
	srv  *http.Server
}

// New builds the admin server.
func New(b Backend, path string) *Server {
	s := &Server{b: b, path: path}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"status": "ok", "version": b.Version()})
	}))
	mux.HandleFunc("/metrics", s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, b.Metrics())
	}))
	mux.HandleFunc("/providers", s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, b.ProvidersView())
	}))
	mux.HandleFunc("/aliases", s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, b.AliasesView())
	}))
	mux.HandleFunc("/budgets", s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, b.BudgetsView())
	}))
	mux.HandleFunc("/health-states", s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, b.HealthView())
	}))
	mux.HandleFunc("/logs", s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, b.RecentRequests(limitParam(r, 100)))
	}))
	mux.HandleFunc("/rollups", s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, b.RecentRollups(limitParam(r, 200)))
	}))
	mux.HandleFunc("/audit", s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, b.RecentAudit(limitParam(r, 100)))
	}))
	mux.HandleFunc("/cache", s.handle(func(w http.ResponseWriter, r *http.Request) {
		hit, miss, evict, size := b.CacheStats()
		writeJSON(w, map[string]any{"hit": hit, "miss": miss, "evict": evict, "size": size})
	}))
	mux.HandleFunc("/config", s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"path": b.ConfigPath(), "version": b.Version()})
	}))
	mux.HandleFunc("/reload", s.handle(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "POST required")
			return
		}
		if err := b.Reload(); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	}))
	s.srv = &http.Server{Handler: mux}
	return s
}

// handle injects CORS-free JSON content type and a read timeout.
func (s *Server) handle(f func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		f(w, r)
	}
}

// Serve listens on the Unix socket until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	_ = os.Remove(s.path) // clear a stale socket.
	ln, err := net.Listen("unix", s.path)
	if err != nil {
		return err
	}
	_ = os.Chmod(s.path, 0o600)
	go func() {
		<-ctx.Done()
		s.srv.Close()
	}()
	return s.srv.Serve(ln)
}

func limitParam(r *http.Request, d int) int {
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return d
}

// Close shuts the admin HTTP server down.
func (s *Server) Close() error {
	if s.srv != nil {
		return s.srv.Close()
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
