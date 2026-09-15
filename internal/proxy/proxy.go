// Package proxy is the client-facing OpenAI-compatible HTTP endpoint: chat
// completions (streaming and non-streaming), model listing, health, plus the
// first-token-buffered retry and exact-match cache pipeline.
package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/cache"
	"github.com/rishabh-yadav11/tallow/internal/compact"
	"github.com/rishabh-yadav11/tallow/internal/observ"
	"github.com/rishabh-yadav11/tallow/internal/registry"
	"github.com/rishabh-yadav11/tallow/internal/routing"
	"github.com/rishabh-yadav11/tallow/internal/store"
)

// Deps is everything the proxy needs; the composition root wires these.
type Deps struct {
	Router        *routing.Router
	Registry      *registry.Registry
	Cache         *cache.Cache
	Store         *store.Store
	Observ        *observ.Collector
	Client        *http.Client
	Auth          func(token string) bool // nil -> allow all (loopback trust).
	MaxConcurrent int
	QueueTimeout  time.Duration
	Compaction    compact.Config
	CacheEnabled  bool
	RawBodies     bool
}

// Handler implements the OpenAI-compatible surface.
type Handler struct {
	deps  Deps
	slots chan struct{}
}

// NewHandler constructs the HTTP handler with a bounded concurrency queue.
func NewHandler(d Deps) *Handler {
	if d.MaxConcurrent <= 0 {
		d.MaxConcurrent = 16
	}
	if d.QueueTimeout <= 0 {
		d.QueueTimeout = 30 * time.Second
	}
	return &Handler{deps: d, slots: make(chan struct{}, d.MaxConcurrent)}
}

// Routes returns the mux for the client-facing API.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", h.chatCompletions)
	mux.HandleFunc("/v1/completions", h.chatCompletions) // legacy single-turn alias.
	mux.HandleFunc("/v1/models", h.listModels)
	mux.HandleFunc("/v1/models/", h.getModel)
	mux.HandleFunc("/health", h.health)
	return mux
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	names := h.deps.Registry.AliasNames()
	data := make([]map[string]any, 0, len(names))
	for _, n := range names {
		data = append(data, map[string]any{"id": n, "object": "model", "owned_by": "tallow"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (h *Handler) getModel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/models/")
	a, ok := h.deps.Registry.Alias(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "invalid_request_error", "model not found")
		return
	}
	ctxWin, tools := 0, false
	for _, t := range a.Targets {
		if t.ContextWindow > ctxWin {
			ctxWin = t.ContextWindow
		}
		tools = tools || t.SupportsTools
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "object": "model", "created": 0, "owned_by": "tallow",
		"context_window": ctxWin, "supports_tools": tools,
	})
}

// auth validates the request's bearer token against the allow-list. A nil
// closure, or an empty allow-list (open/loopback trust), accepts any token
// including a missing header.
func (h *Handler) auth(w http.ResponseWriter, r *http.Request) bool {
	if h.deps.Auth == nil {
		return true
	}
	token, _ := bearerToken(r)
	if h.deps.Auth(token) {
		return true
	}
	writeErr(w, http.StatusUnauthorized, "authentication_error", "invalid api key")
	return false
}

// bearerToken extracts the bearer credential.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer "), true
	}
	return h, true
}

// acquireSlot waits up to QueueTimeout for a concurrency slot (bounded queue).
func (h *Handler) acquireSlot() error {
	timer := time.NewTimer(h.deps.QueueTimeout)
	defer timer.Stop()
	select {
	case h.slots <- struct{}{}:
		return nil
	case <-timer.C:
		return errOverCapacity
	}
}

func (h *Handler) releaseSlot() { <-h.slots }

var errOverCapacity = &overCapacityError{}

type overCapacityError struct{}

func (*overCapacityError) Error() string { return "over capacity" }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": typ}})
}

func randID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
