// Package proxy is the client-facing OpenAI-compatible HTTP endpoint: chat
// completions (streaming and non-streaming), model listing, health, plus the
// first-token-buffered retry and exact-match cache pipeline.
package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

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
	// MaxRequestBytes caps the buffered request body. Zero or negative means
	// unbounded, which is the pre-H6 behaviour and is never the default.
	MaxRequestBytes int64
}

// Handler implements the OpenAI-compatible surface.
type Handler struct {
	deps  Deps
	slots chan struct{}
	// inflight coalesces concurrent cache misses on the same key (H8).
	inflight singleflight.Group
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
//
// H4: auth used to be a call at the TOP of chatCompletions and nowhere else, so
// /v1/models, /v1/models/{id}, and /health answered unauthenticated clients even
// with a populated allow-list, disclosing the full alias inventory and the
// provider topology behind it. It is now middleware wrapped around the whole
// mux, so a new route is authenticated by construction and cannot be added
// unauthenticated by accident.
//
// /health is exempted because a liveness probe that needs a credential cannot
// be used by most orchestrators, and it returns a static "ok" with no tenant
// information. The exemption is explicit and narrow rather than a default.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", h.chatCompletions)
	mux.HandleFunc("/v1/completions", h.chatCompletions) // legacy single-turn alias.
	mux.HandleFunc("/v1/models", h.listModels)
	mux.HandleFunc("/v1/models/", h.getModel)
	mux.HandleFunc("/health", h.health)
	return h.authenticated(mux)
}

// authenticated applies the bearer check to every route except /health.
func (h *Handler) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		if !h.auth(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
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
//
// On success the validated identity is stashed on the request for the cache key
// (C2). On failure the 401 is written here and false is returned; the caller
// must not write a body afterwards.
func (h *Handler) auth(w http.ResponseWriter, r *http.Request) bool {
	if h.deps.Auth == nil {
		// Open gateway: there is no authenticated principal, so the cache
		// namespace falls back to the empty identity. See cacheScope for why
		// that is safe and documented rather than merely convenient.
		setIdentity(r, "")
		return true
	}
	token, _ := bearerToken(r)
	if !h.deps.Auth(token) {
		writeErr(w, http.StatusUnauthorized, "authentication_error", "invalid api key")
		return false
	}
	setIdentity(r, token)
	return true
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

// writeRetryAfter sets a Retry-After header from a routing-supplied wait.
//
// It is a no-op when the wait is unknown, which is the correct answer: the header
// is advisory, and emitting a guess would be worse than omitting it.
//
// The value is whole seconds, rounded UP. Rounding down would invite the client
// back before the window actually rolls, and a client that obeys the header
// would be rate limited again immediately, turning a limit into self-inflicted
// retry load.
//
// The d <= 0 guard above already guarantees the result is at least 1, so there
// is deliberately no separate floor here. An earlier version had both; a
// mutation test deleting the floor survived, which is the signature of a
// redundant guard rather than a missing test.
func writeRetryAfter(w http.ResponseWriter, d time.Duration) {
	if d <= 0 {
		return
	}
	secs := int64((d + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
}

func writeErr(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": typ}})
}

func randID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
