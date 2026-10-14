// Package admin is the stable, minimal control interface. It exposes gateway
// state and controls (metrics, config, live budgets, logs) over HTTP bound to
// a local Unix domain socket. Frontends (the TUI, or any future web UI) talk to
// this interface and never reach into gateway internals.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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

	// ConfigRead returns the live config at the given scope (empty = the full
	// document, otherwise section and optional field) serialized as JSON.
	ConfigRead(scope []string) (json.RawMessage, error)
	// ConfigWrite applies a config mutation at scope. method is PUT (replace),
	// PATCH (merge), or DELETE (remove). The value is persisted to disk and
	// hot-reloaded only when it passes config.Load/Validate; an invalid change
	// is refused without touching the running gateway.
	ConfigWrite(method string, scope []string, body json.RawMessage) error
	// ReplayRequest re-runs the stored request identified by id through the
	// proxy pipeline, writing the original response shape to w.
	ReplayRequest(w http.ResponseWriter, id string) error
	// KeyRefs lists the refs in the encrypted keystore. Only refs are returned,
	// never plaintext secrets.
	KeyRefs() []string
	// UploadKey encrypts plaintext under ref and persists it in the keystore.
	UploadKey(ref string, plaintext []byte) error
	// DeleteKey removes the credential referenced by ref.
	DeleteKey(ref string) error
}

// ErrConfigNotFound is returned by ConfigRead when no section/field exists at the
// requested scope. The admin layer maps it to a 404 instead of a generic error.
var ErrConfigNotFound = errors.New("config: section/field not found")

// ProviderView is the admin/tool view of a provider + its keys.
type ProviderView struct {
	Name    string    `json:"name"`
	BaseURL string    `json:"base_url"`
	RPM     int       `json:"rpm"`
	Health  string    `json:"health"`
	Keys    []KeyView `json:"keys"`
}

// KeyView is a key's live status (secret never exposed). Costs are rendered in
// cents for the operator, converted from the internal micro-USD unit.
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
	mux.HandleFunc("/config", s.handle(s.config))
	mux.HandleFunc("/config/", s.handle(s.config))
	mux.HandleFunc("/keys", s.handle(s.keysCollection))
	mux.HandleFunc("/keys/", s.handle(s.keysElement))
	mux.HandleFunc("/replay/", s.handle(s.replay))
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

// configScope extracts the section/field path components from a /config URL,
// returning ["server"] for /config/server and ["server", "listen"] for
// /config/server/listen. It is nil for the bare /config document.
func configScope(p string) []string {
	s := strings.TrimPrefix(p, "/config")
	s = strings.Trim(s, "/")
	if s == "" {
		return nil
	}
	return strings.Split(s, "/")
}

// config serves GET/PUT/PATCH/DELETE on /config[/section][/field].
//
// GET reads the live config at the requested scope.
// PUT replaces the value at the scope (full document, section, or field).
// PATCH merges the value at the scope.
// DELETE removes a field/section (refused for the full document).
// All writes are validated and hot-reloaded by the backend before answering.
func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	scope := configScope(r.URL.Path)
	switch r.Method {
	case http.MethodGet:
		value, err := s.b.ConfigRead(scope)
		if err != nil {
			switch {
			case errors.Is(err, ErrConfigNotFound):
				writeErr(w, http.StatusNotFound, "config section/field not found")
			default:
				writeErr(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		writeJSON(w, json.RawMessage(value))
	case http.MethodPut, http.MethodPatch:
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
			return
		}
		if len(scope) == 0 && len(raw) == 0 {
			writeErr(w, http.StatusBadRequest, "empty request body")
			return
		}
		if !json.Valid(raw) {
			writeErr(w, http.StatusBadRequest, "request body is not valid JSON")
			return
		}
		if err := s.b.ConfigWrite(r.Method, scope, raw); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	case http.MethodDelete:
		if len(scope) == 0 {
			writeErr(w, http.StatusBadRequest, "cannot delete the whole config document")
			return
		}
		if err := s.b.ConfigWrite(r.Method, scope, nil); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// keysCollection serves GET (list refs) and POST (upload a secret).
func (s *Server) keysCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]any{"refs": s.b.KeyRefs()})
	case http.MethodPost:
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
			return
		}
		var p struct {
			Ref    string `json:"ref"`
			Secret string `json:"secret"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			writeErr(w, http.StatusBadRequest, "request body must be JSON {ref, secret}")
			return
		}
		if p.Ref == "" || p.Secret == "" {
			writeErr(w, http.StatusBadRequest, "both ref and secret are required")
			return
		}
		if err := s.b.UploadKey(p.Ref, []byte(p.Secret)); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "ref": p.Ref})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// keysElement serves DELETE /keys/{ref}.
func (s *Server) keysElement(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ref := strings.TrimPrefix(r.URL.Path, "/keys/")
	if ref == "" {
		writeErr(w, http.StatusBadRequest, "missing key ref")
		return
	}
	if err := s.b.DeleteKey(ref); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// replay serves POST /replay/{id}, re-running a stored request through the
// proxy pipeline and writing the original response shape back to the caller.
func (s *Server) replay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/replay/")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing request id")
		return
	}
	if err := s.b.ReplayRequest(w, id); err != nil {
		// The backend has written a real response on success, so only write an
		// error envelope on the failure paths.
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "request not found: "+id)
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

// handle injects CORS-free JSON content type and a read timeout.
func (s *Server) handle(f func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		defer func() {
			if recover() != nil {
				// A panicking backend (a bug in a metrics/state method) must not
				// take the whole admin server - and with it the gateway process -
				// down. Surface a JSON 500 and let the other endpoints keep
				// serving; the next request to /health must still succeed.
				writeErr(w, http.StatusInternalServerError, "internal error")
			}
		}()
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

// maxLimitParam caps the ?limit= query parameter. The parameter used to be
// passed straight through to a SQL LIMIT, so anything able to open the admin
// socket could ask for 100 million rows in one call and have the gateway build
// and serialise the whole request log to answer it. The admin API is local-only
// by design, but "local" is a weaker boundary than it looks: any process running
// as any user who can reach the socket, including a compromised low-privilege
// process, could turn a read endpoint into an unbounded allocation in the
// gateway's own address space. A cap costs an operator nothing, because asking
// for more than this many rows in one response is not a workflow, it is an
// export, and exports should not run through a status endpoint.
//
// The cap is deliberately generous: far more than any dashboard or TUI view
// renders, so no legitimate consumer notices it.
const maxLimitParam = 1000

// limitParam reads ?limit=, falling back to d for anything missing, malformed
// or out of range, and clamping to maxLimitParam.
func limitParam(r *http.Request, d int) int {
	n := d
	if v := r.URL.Query().Get("limit"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			n = p
		}
	}
	if n > maxLimitParam {
		return maxLimitParam
	}
	return n
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
