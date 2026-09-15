package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/cache"
	"github.com/rishabh-yadav11/tallow/internal/compact"
	"github.com/rishabh-yadav11/tallow/internal/model"
)

// chatCompletions is the core pipeline: auth -> queue -> parse -> compact ->
// exact-match cache -> route -> forward (with first-token retry) -> record.
func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "POST required")
		return
	}
	if !h.auth(w, r) {
		return
	}
	if err := h.acquireSlot(); err != nil {
		writeErr(w, http.StatusTooManyRequests, "rate_limit_error", err.Error())
		return
	}
	defer h.releaseSlot()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "read body")
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	alias, _ := req["model"].(string)
	if alias == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "missing model")
		return
	}
	stream, _ := req["stream"].(bool)

	// Tool-output compaction (distinct pipeline stage from caching).
	if msgs, ok := req["messages"].([]any); ok {
		compact.Apply(msgs, h.deps.Compaction)
	}

	started := time.Now()
	meta := model.RequestMeta{
		ID:        randID(),
		StartedAt: started,
		Model:     alias,
		Stream:    stream,
		Status:    "ok",
	}

	// Exact-match cache: non-streaming only (see README for the tradeoff).
	cacheKey := ""
	if h.deps.CacheEnabled && h.deps.Cache != nil && !stream {
		if canon, err := cache.Normalize(req); err == nil {
			cacheKey = cache.Key(canon)
			if e, ok := h.deps.Cache.Get(cacheKey); ok {
				h.serveCached(w, e, &meta, started)
				return
			}
		}
	}

	sessionKey := h.sessionKey(r, alias)

	skip := map[string]bool{}
	var lastErr error
	maxAttempts := h.maxAttempts(alias)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		sel, err := h.deps.Router.Select(alias, sessionKey, skip)
		if err != nil {
			lastErr = err
			break
		}
		meta.Provider = sel.Provider
		meta.Key = sel.Key
		meta.UpstreamModel = sel.Model
		meta.RouteReason = sel.Reason

		upBody := h.upstreamBody(req, sel.Model)
		res, ferr := h.forward(w, r, sel, upBody, stream)

		if ferr == nil {
			meta.PromptTokens = res.prompt
			meta.CompletionTokens = res.completion
			meta.CostCents = costCents(sel.Target, res.prompt, res.completion)
			h.deps.Router.RecordSuccess(sel)
			h.deps.Router.Release(sel, meta.CostCents)
			h.record(meta, res.respBody, body, started)
			// Populate cache on success (non-stream, non-empty key).
			if cacheKey != "" && h.deps.Cache != nil {
				h.deps.Cache.Set(cache.Entry{
					Key:              cacheKey,
					Body:             res.respBody,
					ContentType:      "application/json",
					Status:           200,
					Model:            sel.Model,
					PromptTokens:     res.prompt,
					CompletionTokens: res.completion,
				})
			}
			return
		}

		// Failure. Release the reserved slot and account toward the breaker.
		h.deps.Router.Release(sel, 0)
		h.deps.Router.RecordFailure(sel)
		skip[sel.Provider+"/"+sel.Key] = true
		if ferr.transport {
			skip[sel.Provider] = true
		}

		if ferr.streamStarted {
			// Streaming already begun: never splice recovery — let the client
			// observe the interruption (deliberate tradeoff).
			meta.Status = "error"
			meta.Err = ferr.Error()
			h.record(meta, nil, body, started)
			return
		}
		if !ferr.retryable {
			meta.Status = "error"
			meta.Err = ferr.Error()
			h.record(meta, nil, body, started)
			st := ferr.status
			if st == 0 {
				st = http.StatusBadGateway
			}
			writeErr(w, st, "upstream_error", ferr.Error())
			return
		}
		lastErr = ferr
	}

	// No route or exhausted retries.
	meta.Status = "error"
	if lastErr != nil {
		meta.Err = lastErr.Error()
	} else {
		meta.Err = "no route"
	}
	h.record(meta, nil, body, started)
	writeErr(w, http.StatusBadGateway, "upstream_error", meta.Err)
}

// sessionKey builds the sticky-routing key: X-Session-Id header when present,
// else (client bearer token + alias) affinity.
func (h *Handler) sessionKey(r *http.Request, alias string) string {
	if sid := r.Header.Get("X-Session-Id"); sid != "" {
		return "sess:" + sid + ":" + alias
	}
	token, _ := bearerToken(r)
	return "aff:" + token + ":" + alias
}

// upstreamBody replaces the client-facing alias with the selected upstream
// model string, preserving every other field (including cache-control markers).
func (h *Handler) upstreamBody(req map[string]any, upstreamModel string) []byte {
	req["model"] = upstreamModel
	b, _ := json.Marshal(req)
	return b
}

// maxAttempts bounds the retry loop to the number of provider/key candidates.
func (h *Handler) maxAttempts(alias string) int {
	if a, ok := h.deps.Registry.Alias(alias); ok {
		n := 0
		for _, t := range a.Targets {
			if p, ok := h.deps.Registry.Provider(t.Provider); ok {
				n += len(p.Keys)
			}
		}
		if n > 1 {
			return n
		}
	}
	return 2
}

// serveCached emits a cached non-streaming response.
func (h *Handler) serveCached(w http.ResponseWriter, e cache.Entry, meta *model.RequestMeta, started time.Time) {
	meta.Status = "cached"
	meta.Cached = true
	meta.Provider = "cache"
	meta.RouteReason = "cache_hit"
	meta.PromptTokens = e.PromptTokens
	meta.CompletionTokens = e.CompletionTokens
	w.Header().Set("Content-Type", e.ContentType)
	w.WriteHeader(e.Status)
	_, _ = w.Write(e.Body)
	h.record(*meta, nil, nil, started)
}

// record persists metadata + raw bodies and folds live observability.
func (h *Handler) record(meta model.RequestMeta, respBody, reqBody []byte, started time.Time) {
	meta.DurMillis = time.Since(started).Milliseconds()
	if h.deps.Store != nil {
		_ = h.deps.Store.RecordRequest(meta)
		if h.deps.RawBodies {
			_ = h.deps.Store.RecordRaw(meta.ID, meta.StartedAt, reqBody, respBody)
		}
		if meta.Status == "error" && meta.Err != "" {
			_ = h.deps.Store.RecordError(meta.ID, meta.Provider, meta.Key, meta.Model, "", meta.Err)
		}
	}
	if h.deps.Observ != nil {
		h.deps.Observ.Record(meta, meta.DurMillis)
	}
}

// costCents computes cost from manually-declared per-M pricing.
func costCents(t model.Target, prompt, completion int) int64 {
	if t.PriceInputPerM == 0 && t.PriceOutputPerM == 0 {
		return 0
	}
	usd := (float64(prompt)*t.PriceInputPerM + float64(completion)*t.PriceOutputPerM) / 1e6
	return int64(usd * 100)
}
