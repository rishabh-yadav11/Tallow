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

	GetConfigDoc(root string) ([]byte, error)

	PutConfigDoc(root string, body []byte) error

	PatchConfigDoc(root string, body []byte) error

	DeleteConfigDoc(root string) error

	ListKeyRefs() []string

	SetKeyRef(ref, value string) error

	DeleteKeyRef(ref string) error

	ReplayRequest(id string) ([]byte, error)
}

var ErrReplayUnavailable = errors.New("raw request bodies were not captured")

var ErrReplayNotFound = errors.New("no such request")

type ProviderView struct {
	Name    string    `json:"name"`
	BaseURL string    `json:"base_url"`
	RPM     int       `json:"rpm"`
	Health  string    `json:"health"`
	Keys    []KeyView `json:"keys"`
}

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

type AliasView struct {
	Name    string       `json:"name"`
	Targets []TargetView `json:"targets"`
}

type TargetView struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type Server struct {
	b    Backend
	path string
	srv  *http.Server
}

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
	mux.HandleFunc("/config", s.handle(s.serveConfigRoot))
	mux.HandleFunc("/config/", s.handle(s.serveConfigTree))
	mux.HandleFunc("/replay/", s.handle(s.serveReplay))
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

func (s *Server) serveConfigRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		doc, err := s.b.GetConfigDoc("")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Write(doc)
	case http.MethodPut:
		s.applyConfig(w, r, "", http.MethodPut)
	case http.MethodPatch:
		s.applyConfig(w, r, "", http.MethodPatch)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "GET, PUT or PATCH required")
	}
}

func (s *Server) serveConfigTree(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/config")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		s.serveConfigRoot(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	if parts[0] == "keys" {
		s.serveKeyRefs(w, r, parts[1:])
		return
	}
	switch r.Method {
	case http.MethodGet:
		doc, err := s.b.GetConfigDoc(rest)
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		w.Write(doc)
	case http.MethodPut:
		s.applyConfig(w, r, rest, http.MethodPut)
	case http.MethodPatch:
		s.applyConfig(w, r, rest, http.MethodPatch)
	case http.MethodDelete:
		if err := s.b.DeleteConfigDoc(rest); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) applyConfig(w http.ResponseWriter, r *http.Request, root, method string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var applyErr error
	switch method {
	case http.MethodPut:
		applyErr = s.b.PutConfigDoc(root, body)
	case http.MethodPatch:
		applyErr = s.b.PatchConfigDoc(root, body)
	}
	if applyErr != nil {
		writeErr(w, http.StatusBadRequest, applyErr.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) serveKeyRefs(w http.ResponseWriter, r *http.Request, ref []string) {
	switch {
	case len(ref) == 0 && r.Method == http.MethodGet:
		writeJSON(w, s.b.ListKeyRefs())
	case len(ref) == 0 && r.Method == http.MethodPost:
		var req struct {
			Ref   string `json:"ref"`
			Value string `json:"value"`
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		if req.Ref == "" || req.Value == "" {
			writeErr(w, http.StatusBadRequest, "ref and value are required")
			return
		}
		if err := s.b.SetKeyRef(req.Ref, req.Value); err != nil {

			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	case len(ref) == 1 && r.Method == http.MethodDelete:
		if err := s.b.DeleteKeyRef(ref[0]); err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) serveReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/replay/")
	body, err := s.b.ReplayRequest(id)
	switch {
	case errors.Is(err, ErrReplayNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, ErrReplayUnavailable):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Write(body)
}

func (s *Server) handle(f func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		defer func() {
			if recover() != nil {

				writeErr(w, http.StatusInternalServerError, "internal error")
			}
		}()
		f(w, r)
	}
}

func (s *Server) Serve(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	_ = os.Remove(s.path)
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

const maxLimitParam = 1000

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
