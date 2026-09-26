package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/cache"
	"github.com/rishabh-yadav11/tallow/internal/compact"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/money"
	"github.com/rishabh-yadav11/tallow/internal/routing"
)

// maxRequestBytesDefault caps the buffered request body when no explicit limit
// is configured (H6).
//
// The handler used to call io.ReadAll(r.Body) with no bound, so a single
// 256 MiB POST drove gateway RSS from 13 MB to 587 MB: the whole body was
// buffered in memory before the JSON parse rejected it. 8 MiB is far above any
// realistic chat completion (a 1M-token context is roughly 4 MB of text) and
// small enough that a hostile client cannot exhaust a small deployment.
const maxRequestBytesDefault = 8 << 20

// identityKey carries the validated auth principal on the request context so
// the cache key can be scoped to it (C2).
type identityKey struct{}

// anonymousScope namespaces cached entries when the gateway is open (no
// allow-list). See cacheScope.
const anonymousScope = "open"

// setIdentity records the authenticated principal on the request so the cache
// key can be qualified by it.
func setIdentity(r *http.Request, token string) {
	*r = *r.WithContext(context.WithValue(r.Context(), identityKey{}, token))
}

// identityFrom returns the principal stashed by auth.
func identityFrom(r *http.Request) string {
	v, _ := r.Context().Value(identityKey{}).(string)
	return v
}

// chatCompletions is the core pipeline: auth -> queue -> parse -> compact ->
// exact-match cache -> route -> forward (with first-token retry) -> record.
//
// Auth is applied by the Routes middleware (H4) rather than here, so there is a
// single enforcement point instead of one per handler that a new route can
// forget.
func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "POST required")
		return
	}
	if err := h.acquireSlot(); err != nil {
		writeErr(w, http.StatusTooManyRequests, "rate_limit_error", err.Error())
		return
	}
	defer h.releaseSlot()

	body, ok := h.readBody(w, r)
	if !ok {
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
	sessionKey := h.sessionKey(r, alias)
	// Encode the full request once. Every attempt patches only the model field
	// into this buffer rather than re-encoding the map (M3).
	base := baseBody(req)

	// Exact-match cache: non-streaming only (see README for the tradeoff).
	//
	// The cacheable path is deliberately separated from the streaming path. The
	// cached path buffers its upstream response, which is what makes it safe to
	// coalesce concurrent misses (H8): one upstream call can serve every caller
	// waiting on the same key, because each of them writes the bytes to its own
	// ResponseWriter. The streaming path cannot do that, because a single
	// response body cannot be replayed to N clients.
	canon, cacheable := h.cacheKey(r, req, alias, stream)
	if cacheable {
		if e, hit := h.deps.Cache.Get(canon); hit {
			// The entry remembers the route that produced it, so a cache hit can
			// still say which provider served it without being attributed to one
			// as though it had just been called.
			h.serveCached(w, e, &meta, started, e.Provider)
			return
		}
		f := h.fetchCached(canon, func() (any, error) {
			return h.routeAndBuffer(r, alias, sessionKey, base)
		})
		if f.err != nil {
			h.failRequest(w, &meta, f, body, started)
			return
		}
		meta.Provider = f.sel.Provider
		meta.Key = f.sel.Key
		meta.UpstreamModel = f.sel.Model
		meta.RouteReason = f.sel.Reason
		// The reservation was already settled by routeAndBuffer, once, inside
		// the flight (H8). Repeating it here would either double-count cost or
		// credit the provider's breaker once per coalesced waiter.
		//
		// A follower did not issue an upstream call, so it reports no spend and
		// no tokens. The leader's row carries both. This keeps the store's
		// rollups and the live totals equal to the money actually spent, instead
		// of the money spent multiplied by the number of callers that shared it.
		if f.leader {
			meta.PromptTokens = f.prompt
			meta.CompletionTokens = f.completion
			meta.CostMicros = f.costMicros
		} else {
			meta.Coalesced = true
			meta.RouteReason = "coalesced_" + f.sel.Reason
		}
		// Only the leader populates the cache. A follower writing the same
		// entry again would be harmless for content but would restart the TTL
		// on every waiter, so a burst of reads could keep a stale entry alive
		// indefinitely.
		if f.leader {
			h.deps.Cache.Set(f.entry())
		}
		h.serveFetched(w, &meta, f, body, started)
		return
	}

	h.serveUncached(w, r, alias, sessionKey, base, body, stream, &meta, started)
}

// readBody reads the request body under the H6 size cap.
//
// MaxBytesReader is installed BEFORE the read, so the limit is enforced during
// buffering rather than after the memory has already been spent. A body over
// the cap is reported as 413 with the actual limit so the client can act on it,
// instead of a 400 that reads like malformed JSON.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	limit := h.deps.MaxRequestBytes
	if limit <= 0 {
		limit = maxRequestBytesDefault
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
				"request body exceeds "+strconv.FormatInt(limit, 10)+" bytes")
			return nil, false
		}
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "read body")
		return nil, false
	}
	return body, true
}

// serveUncached handles streaming requests and requests when the cache is off
// or the alias maps to several models.
func (h *Handler) serveUncached(
	w http.ResponseWriter, r *http.Request, alias, sessionKey string,
	base, body []byte, stream bool, meta *model.RequestMeta, started time.Time,
) {
	skip := map[string]bool{}
	maxAttempts := h.maxAttempts(alias)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		sel, err := h.deps.Router.Select(alias, sessionKey, skip)
		if err != nil {
			// M9: there is no provider to attribute a routing failure to, so
			// none is invented. The request is still counted in the global
			// totals and the error is still stored; only the per-provider
			// bucket is skipped, so a gateway-level routing failure cannot hide
			// behind a real provider's healthy error rate.
			meta.Status = "error"
			meta.Err = err.Error()
			h.record(*meta, nil, body, started)
			writeErr(w, http.StatusBadGateway, "upstream_error", meta.Err)
			return
		}
		meta.Provider = sel.Provider
		meta.Key = sel.Key
		meta.UpstreamModel = sel.Model
		meta.RouteReason = sel.Reason

		res, ferr := h.forward(w, r, sel, upstreamBody(base, sel.Model), stream)
		if ferr == nil {
			meta.PromptTokens = res.prompt
			meta.CompletionTokens = res.completion
			meta.CostMicros = costMicros(sel.Target, res.prompt, res.completion)
			h.deps.Router.RecordSuccess(sel)
			h.deps.Router.Release(sel, meta.CostMicros)
			h.record(*meta, res.respBody, body, started)
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
			// Streaming already begun: never splice recovery - let the client
			// observe the interruption (deliberate tradeoff).
			meta.Status = "error"
			meta.Err = ferr.Error()
			h.record(*meta, nil, body, started)
			return
		}
		if !ferr.retryable {
			meta.Status = "error"
			meta.Err = ferr.Error()
			h.record(*meta, nil, body, started)
			st := ferr.status
			if st == 0 {
				st = http.StatusBadGateway
			}
			writeErr(w, st, "upstream_error", ferr.Error())
			return
		}
	}

	meta.Status = "error"
	meta.Err = "upstream attempts exhausted"
	h.record(*meta, nil, body, started)
	writeErr(w, http.StatusBadGateway, "upstream_error", meta.Err)
}

// fetch is the outcome of a buffered, cacheable upstream fetch.
type fetch struct {
	sel        *routing.Selection
	cacheKey   string
	body       []byte
	status     int
	prompt     int
	completion int
	costMicros int64
	// attempts counts how many upstream tries the flight made. attempts == 0
	// with a nil sel means routing never found a provider, which is reported
	// without a provider attribution (M9).
	attempts int
	err      *forwardError
	// leader is true for the single caller whose flight ran the upstream work.
	// Every other caller received a copy of the result and must not repeat the
	// side effects that belong to that one upstream call (H8).
	leader bool
}

// entry renders the fetch as a cache entry.
func (f fetch) entry() cache.Entry {
	return cache.Entry{
		Key:              f.cacheKey,
		Body:             f.body,
		ContentType:      "application/json",
		Status:           f.status,
		Model:            f.sel.Model,
		PromptTokens:     f.prompt,
		CompletionTokens: f.completion,
		// The route that produced this response, so a later cache hit can report
		// where the answer came from without claiming that provider served it now.
		Provider: f.sel.Provider,
	}
}

// fetchCached coalesces concurrent misses on one cache key (H8).
//
// Before the fix this was a plain check-then-act: 40 identical concurrent POSTs
// to one hot key produced 40 upstream calls at peak concurrency 40, so a hot key
// amplified load on the provider at exactly the moment the provider could least
// afford it. The leader performs the upstream work once; every follower waits
// and receives the same bytes, which it writes to its own ResponseWriter.
//
// The upstream call is buffered rather than streamed, so sharing it is sound.
//
// Accounting is the subtle part. The router's selection carries a reservation
// (an in-flight concurrency slot and a pending cost) that Select acquired once
// and that must be settled exactly once. Release is already idempotent, but
// RecordSuccess is not: it re-affirms the provider and key breakers. If every
// waiter ran both, a burst of 40 coalesced callers would apply 40 successes to
// one upstream call, which would let a provider's breaker look healthy on
// evidence from a single request. Both are therefore performed by the leader,
// inside the flight, where they happen once per real upstream call.
//
// The per-request bookkeeping (meta, the store row) deliberately stays OUTSIDE
// the flight and still runs once per caller, so a coalesced follower is a
// first-class request with its own store row, its own latency, and its own
// cache-hit accounting.
//
// Leadership is not taken from singleflight's third return, which is `shared`
// and means "the result had followers" rather than "I did the work". It is
// observed from inside the closure, which singleflight runs exactly once and
// only in the caller that actually performed the upstream call.
func (h *Handler) fetchCached(key string, do func() (any, error)) fetch {
	leader := false
	shared, _, _ := h.inflight.Do(key, func() (any, error) {
		leader = true
		return do()
	})
	f, _ := shared.(fetch)
	f.cacheKey = key
	f.leader = leader
	return f
}

// routeAndBuffer runs the full retry loop for a cacheable request and returns
// the buffered response.
//
// It deliberately takes no ResponseWriter and touches no per-request metadata:
// those belong to the caller, and two callers sharing one flight must not share
// one store row. What it DOES own is the router reservation. Select acquired a
// concurrency slot and opened a pending-cost accrual for the selected key, and
// that reservation belongs to the upstream call rather than to the HTTP
// requester. Settling it here, inside the flight, is what makes it happen once
// per real upstream call no matter how many callers coalesced onto it (H8).
func (h *Handler) routeAndBuffer(
	r *http.Request, alias, sessionKey string, base []byte,
) (any, error) {
	skip := map[string]bool{}
	maxAttempts := h.maxAttempts(alias)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		sel, err := h.deps.Router.Select(alias, sessionKey, skip)
		if err != nil {
			// No provider was ever selected, so there is nothing to attribute
			// the failure to. M9: this is reported without a provider rather
			// than under a synthetic one.
			return fetch{err: &forwardError{msg: err.Error()}}, nil
		}
		res, ferr := h.fetchBuffered(r, sel, upstreamBody(base, sel.Model))
		if ferr == nil {
			cost := costMicros(sel.Target, res.prompt, res.completion)
			// Settle the reservation exactly once, here, on the path that
			// actually made the upstream call.
			h.deps.Router.RecordSuccess(sel)
			h.deps.Router.Release(sel, cost)
			return fetch{
				sel:        sel,
				body:       res.respBody,
				status:     200,
				prompt:     res.prompt,
				completion: res.completion,
				costMicros: cost,
				attempts:   attempt + 1,
			}, nil
		}
		h.deps.Router.Release(sel, 0)
		h.deps.Router.RecordFailure(sel)
		skip[sel.Provider+"/"+sel.Key] = true
		if ferr.transport {
			skip[sel.Provider] = true
		}
		if !ferr.retryable {
			f := fetch{sel: sel, err: ferr, attempts: attempt + 1}
			return f, nil
		}
	}
	return fetch{err: &forwardError{msg: "upstream attempts exhausted"}}, nil
}

// fetchBuffered performs one upstream request without writing to a client, so
// the result can be shared by every caller coalesced onto the same flight.
func (h *Handler) fetchBuffered(r *http.Request, sel *routing.Selection, body []byte) (result, *forwardError) {
	resp, ferr := h.send(r, sel, body)
	if ferr != nil {
		return result{}, ferr
	}
	defer resp.Body.Close()
	b, ferr := readUpstreamBody(resp)
	if ferr != nil {
		return result{}, ferr
	}
	prompt, completion := parseUsage(b)
	return result{prompt: prompt, completion: completion, respBody: b, status: resp.StatusCode}, nil
}

// serveFetched writes a coalesced fetch result to this caller's client and
// records it.
func (h *Handler) serveFetched(w http.ResponseWriter, meta *model.RequestMeta, f fetch, body []byte, started time.Time) {
	meta.Status = "ok"
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.status)
	_, _ = w.Write(f.body)
	h.record(*meta, f.body, body, started)
}

// failRequest reports a failed cacheable fetch. A fetch that never selected a
// provider carries no provider attribution (M9); one that did is attributed to
// it, so a real provider's error rate still reflects real provider failures.
func (h *Handler) failRequest(
	w http.ResponseWriter, meta *model.RequestMeta, f fetch, body []byte, started time.Time,
) {
	meta.Status = "error"
	meta.Err = f.err.Error()
	if f.sel != nil {
		meta.Provider = f.sel.Provider
		meta.Key = f.sel.Key
		meta.UpstreamModel = f.sel.Model
		meta.RouteReason = f.sel.Reason
	}
	h.record(*meta, nil, body, started)
	st := f.err.status
	if st == 0 {
		st = http.StatusBadGateway
	}
	writeErr(w, st, "upstream_error", f.err.Error())
}

// cacheKey returns the tenant-qualified cache key for this request, and whether
// the request is cacheable at all.
//
// C2: the key used to be model + sha256(normalizedBody), with nothing tying it
// to the caller. Two different API keys sending a byte-identical body therefore
// shared a cache entry, so Bob was served a response generated and billed under
// Alice's identity for any shared prompt - a tenant-isolation break. The
// authenticated principal is now a distinct hashed component, so two callers
// can only collide if they present the same credential, at which point they are
// the same tenant by definition.
//
// The cache is used only when the alias resolves to a single upstream model, so
// a cached response is never served across distinct models behind one alias.
func (h *Handler) cacheKey(r *http.Request, req map[string]any, alias string, stream bool) (string, bool) {
	if !h.deps.CacheEnabled || h.deps.Cache == nil || stream {
		return "", false
	}
	canon, err := cache.Normalize(req)
	if err != nil {
		return "", false
	}
	m, ok := h.singleModel(alias)
	if !ok {
		return "", false
	}
	return cacheScope(m, identityFrom(r), cache.Key(canon)), true
}

// cacheScope builds the tenant-qualified cache key.
//
// The token is hashed rather than embedded, so the cache key - and anything
// that echoes it, such as a diagnostic dump - never contains a credential in
// the clear.
func cacheScope(model, identity, bodyHash string) string {
	if identity == "" {
		// An open gateway has a single effective principal, so a shared cache
		// is the intended behaviour there. The scope is named explicitly so
		// these entries can never be confused with an authenticated tenant's.
		return model + "/" + anonymousScope + "/" + bodyHash
	}
	sum := sha256.Sum256([]byte(identity))
	return model + "/" + hex.EncodeToString(sum[:16]) + "/" + bodyHash
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

// baseBody renders the full request map exactly once per request.
//
// It deliberately does NOT use cache.Normalize, which strips volatile fields
// including `stream`. The upstream needs the complete request: sending it a
// body with `stream` removed turns a streaming completion into a non-streaming
// one and silently breaks the first-token retry path.
func baseBody(req map[string]any) []byte {
	b, err := json.Marshal(req)
	if err != nil {
		// req came from a successful json.Unmarshal into map[string]any, so a
		// marshal failure is not reachable.
		return []byte(`{}`)
	}
	return b
}

// upstreamBody renders base with the model field set to the selected upstream
// model, WITHOUT mutating the caller's map (M3).
//
// M3 had two halves. The old code assigned req["model"] = upstreamModel in
// place, so a retry to a different provider silently requested the PREVIOUS
// provider's model name, because the alias had already been destroyed by the
// first attempt. It also re-marshalled the whole map on every attempt, so each
// retry paid a full JSON encode of the entire request.
//
// Here the map is encoded once by baseBody and each attempt only patches the
// model field, so the per-attempt cost is a scan plus one concatenation rather
// than a full re-encode. The model string is escaped with json.Marshal before
// splicing, so a model name containing a quote or backslash cannot produce
// invalid JSON - which a hand-built string concatenation could not promise.
func upstreamBody(base []byte, upstreamModel string) []byte {
	return spliceTopLevelModel(base, upstreamModel)
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

// singleModel returns the upstream model an alias resolves to, or ("", false)
// when the alias can route to multiple distinct models. The exact-match cache
// is only served/populated for single-model aliases so a cached response is
// never served across distinct models behind the same alias.
func (h *Handler) singleModel(alias string) (string, bool) {
	a, ok := h.deps.Registry.Alias(alias)
	if !ok || len(a.Targets) == 0 {
		return "", false
	}
	m := a.Targets[0].Model
	for _, t := range a.Targets[1:] {
		if t.Model != m {
			return "", false
		}
	}
	return m, true
}

// serveCached emits a cached non-streaming response.
//
// M7: this used to copy the ORIGINAL request's token counts into the live totals
// while leaving CostMicros at 0, so the token and cost aggregates disagreed by
// construction and the disagreement grew as the cache warmed. A cache hit
// consumed no provider tokens and no provider money, so it now reports zero for
// both. The number of served-from-cache requests is still tracked, so cache
// effectiveness is not lost, and the token counts remain available on the cache
// entry itself for anyone who wants the served volume.
//
// The route attribution is the other half of M7 and the same class of defect as
// M9. This used to set Provider = "cache", inventing a provider that no
// operator ever configured. It appeared in by_provider and in the TUI's
// provider list as a peer of the real providers, and it grew without bound as the
// cache warmed: a bucket that is neither a provider nor bounded by any provider
// limit dilutes the provider picture exactly when it stops being a useful
// signal.
//
// A cache hit genuinely did not come from a provider, so attributing it to one
// would be a lie. Provider is left empty and Cached is set instead, which is the
// same truthful representation M9 settled on for a routing failure: counted in
// the global totals, absent from the per-provider view. The route a cached
// response CAME FROM is still recorded, as RouteReason, so the cache row in the
// log still shows which route served it.
func (h *Handler) serveCached(w http.ResponseWriter, e cache.Entry, meta *model.RequestMeta, started time.Time, servedBy string) {
	meta.Status = "cached"
	meta.Cached = true
	meta.Provider = ""
	meta.RouteReason = "cache_hit"
	if servedBy != "" {
		meta.RouteReason = "cache_hit_from:" + servedBy
	}
	meta.PromptTokens = 0
	meta.CompletionTokens = 0
	meta.CostMicros = 0
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

// costMicros computes cost in micro-USD from manually-declared per-M pricing.
//
// Cost was previously accumulated in integer cents, which truncated toward
// zero: a request costing $0.002 recorded 0 cents, so cost budgets could never
// fire and every total and rollup read zero. Micro-USD (1 USD = 1,000,000)
// keeps sub-cent spend intact; the value is converted to cents only for
// operator-facing displays.
func costMicros(t model.Target, prompt, completion int) int64 {
	return money.CostMicros(t.PriceInputPerM, t.PriceOutputPerM, prompt, completion)
}

// spliceTopLevelModel replaces the top-level "model" value in canonical JSON
// without re-encoding the document.
//
// encoding/json emits map keys in sorted order, so the top-level "model" member
// is locatable with an exact byte match. Nested objects and arrays are skipped
// by tracking depth, so a user message containing the literal text
// "model":"something" is never mistaken for the top-level field.
func spliceTopLevelModel(canonical []byte, upstreamModel string) []byte {
	enc, err := json.Marshal(upstreamModel)
	if err != nil {
		return canonical
	}
	needle := []byte(`"model":`)
	keyStart, valStart, valEnd := topLevelStringField(canonical, needle)
	if keyStart < 0 {
		// No model field at all: the caller has already rejected that case, so
		// this is unreachable. Return the input rather than inventing a body.
		return canonical
	}
	_ = keyStart
	out := make([]byte, 0, len(canonical)-(valEnd-valStart)+len(enc))
	out = append(out, canonical[:valStart]...)
	out = append(out, enc...)
	out = append(out, canonical[valEnd:]...)
	return out
}

// topLevelStringField finds a top-level string-valued member whose key is the
// given already-quoted needle. It returns the value's start and end offsets.
func topLevelStringField(canonical []byte, needle []byte) (keyStart, valStart, valEnd int) {
	depth := 0
	for i := 0; i < len(canonical); i++ {
		switch canonical[i] {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case '"':
			end := skipJSONString(canonical, i)
			if depth == 1 && hasPrefixAt(canonical, i, needle) {
				v := end + 1
				for v < len(canonical) && canonical[v] != ':' {
					v++
				}
				v++
				for v < len(canonical) && (canonical[v] == ' ' || canonical[v] == '\t') {
					v++
				}
				return i, v, jsonValueEnd(canonical, v)
			}
			i = end
		}
	}
	return -1, -1, -1
}

func hasPrefixAt(b []byte, i int, needle []byte) bool {
	return i+len(needle) <= len(b) && string(b[i:i+len(needle)]) == string(needle)
}

// skipJSONString returns the offset of the closing quote of the string starting
// at i, honouring backslash escapes.
func skipJSONString(b []byte, i int) int {
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j
		}
	}
	return len(b) - 1
}

// jsonValueEnd returns the offset just past the JSON value starting at i.
func jsonValueEnd(b []byte, i int) int {
	if i >= len(b) {
		return len(b)
	}
	switch b[i] {
	case '"':
		return skipJSONString(b, i) + 1
	case '{', '[':
		depth := 0
		for j := i; j < len(b); j++ {
			switch b[j] {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1
				}
			case '"':
				j = skipJSONString(b, j)
			}
		}
		return len(b)
	}
	for j := i; j < len(b); j++ {
		switch b[j] {
		case ',', '}', ']':
			return j
		}
	}
	return len(b)
}
