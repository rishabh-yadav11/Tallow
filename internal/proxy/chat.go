package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/cache"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/money"
	"github.com/rishabh-yadav11/tallow/internal/resilience"
	"github.com/rishabh-yadav11/tallow/internal/routing"
)

const maxRequestBytesDefault = 8 << 20

type identityKey struct{}

const anonymousScope = "open"

func setIdentity(r *http.Request, token string) {
	*r = *r.WithContext(context.WithValue(r.Context(), identityKey{}, token))
}

func identityFrom(r *http.Request) string {
	v, _ := r.Context().Value(identityKey{}).(string)
	return v
}

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

func (h *Handler) rulesFor(sel *routing.Selection) []model.ErrorRule {
	if sel == nil || h.deps.Registry == nil {
		return nil
	}
	p, ok := h.deps.Registry.Provider(sel.Provider)
	if !ok {
		return nil
	}
	for i := range p.Keys {
		if p.Keys[i].ID == sel.Key {
			return p.Keys[i].ErrorRules
		}
	}
	return nil
}

func (h *Handler) decideRetry(sel *routing.Selection, ferr *forwardError) bool {
	if h.deps.Resilience == nil {
		return ferr.retryable
	}
	var err error
	if ferr.status == 0 && ferr.retryable {
		err = ferr
	}
	action := h.deps.Resilience.Decide(sel.Provider, sel.Key, ferr.status, err, ferr.body, h.rulesFor(sel))
	return action == resilience.ActionContinue || action == resilience.ActionContinueAndCooldown
}

func (h *Handler) serveUncached(
	w http.ResponseWriter, r *http.Request, alias, sessionKey string,
	base, body []byte, stream bool, meta *model.RequestMeta, started time.Time,
) {
	skip := map[string]bool{}
	maxAttempts := h.maxAttempts(alias)

	var attemptErrs []string
	for attempt := 0; attempt < maxAttempts; attempt++ {
		sel, err := h.deps.Router.Select(alias, sessionKey, skip)
		if err != nil {

			meta.Status = "error"
			meta.Err = err.Error()
			h.record(*meta, nil, body, started)

			if errors.Is(err, routing.ErrNoRoute) {
				var re *routing.RouteError
				if errors.As(err, &re) && re.Limited {

					writeRetryAfter(w, re.RetryAfter)
					writeErr(w, http.StatusTooManyRequests, "rate_limit_error", meta.Err)
					return
				}
			}
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

		h.deps.Router.Release(sel, 0)
		h.deps.Router.RecordFailure(sel)
		skip[sel.Provider+"/"+sel.Key] = true
		if ferr.transport {
			skip[sel.Provider] = true
		}
		attemptErrs = append(attemptErrs, attemptReason(sel, ferr))
		logTransportFailure(sel, ferr)

		if ferr.streamStarted {

			meta.Status = "error"
			meta.Err = ferr.Error()
			h.record(*meta, nil, body, started)
			return
		}
		if !h.decideRetry(sel, ferr) {
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
	meta.Err = exhaustedMessage(attemptErrs)
	h.record(*meta, nil, body, started)
	writeErr(w, http.StatusBadGateway, "upstream_error", meta.Err)
}

func attemptReason(sel *routing.Selection, ferr *forwardError) string {
	return sel.Provider + "/" + sel.Key + ": " + ferr.Error()
}

func logTransportFailure(sel *routing.Selection, ferr *forwardError) {
	if !ferr.transport || ferr.logDetail() == ferr.Error() {
		return
	}
	slog.Error("upstream transport failure",
		"provider", sel.Provider,
		"key", sel.Key,
		"detail", ferr.logDetail())
}

func exhaustedMessage(attemptErrs []string) string {
	const maxReported = 4
	if len(attemptErrs) == 0 {
		return "upstream attempts exhausted"
	}
	shown := attemptErrs
	suffix := ""
	if len(shown) > maxReported {
		shown = shown[:maxReported]
		suffix = fmt.Sprintf(" (+%d more)", len(attemptErrs)-maxReported)
	}
	return fmt.Sprintf("upstream attempts exhausted after %d: %s%s",
		len(attemptErrs), strings.Join(shown, "; "), suffix)
}

type fetch struct {
	sel        *routing.Selection
	cacheKey   string
	body       []byte
	status     int
	prompt     int
	completion int
	costMicros int64

	attempts int
	err      *forwardError

	leader bool
}

func (f fetch) entry() cache.Entry {
	return cache.Entry{
		Key:              f.cacheKey,
		Body:             f.body,
		ContentType:      "application/json",
		Status:           f.status,
		Model:            f.sel.Model,
		PromptTokens:     f.prompt,
		CompletionTokens: f.completion,

		Provider: f.sel.Provider,
	}
}

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

func (h *Handler) routeAndBuffer(
	r *http.Request, alias, sessionKey string, base []byte,
) (any, error) {
	skip := map[string]bool{}
	maxAttempts := h.maxAttempts(alias)

	var attemptErrs []string
	for attempt := 0; attempt < maxAttempts; attempt++ {
		sel, err := h.deps.Router.Select(alias, sessionKey, skip)
		if err != nil {

			fe := &forwardError{msg: err.Error()}
			if errors.Is(err, routing.ErrNoRoute) {
				var re *routing.RouteError
				if errors.As(err, &re) && re.Limited {
					fe.status = http.StatusTooManyRequests

					fe.retryAfter = re.RetryAfter
				}
			}
			return fetch{err: fe}, nil
		}
		res, ferr := h.fetchBuffered(r, sel, upstreamBody(base, sel.Model))
		if ferr == nil {
			cost := costMicros(sel.Target, res.prompt, res.completion)

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
		attemptErrs = append(attemptErrs, attemptReason(sel, ferr))
		if !h.decideRetry(sel, ferr) {
			f := fetch{sel: sel, err: ferr, attempts: attempt + 1}
			return f, nil
		}
	}
	return fetch{err: &forwardError{msg: exhaustedMessage(attemptErrs)}}, nil
}

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
	if sel.Target.ForceMapping && sel.Alias != "" {
		b = spliceTopLevelModel(b, sel.Alias)
	}
	prompt, completion := parseUsage(b)
	return result{prompt: prompt, completion: completion, respBody: b, status: resp.StatusCode}, nil
}

func (h *Handler) serveFetched(w http.ResponseWriter, meta *model.RequestMeta, f fetch, body []byte, started time.Time) {
	meta.Status = "ok"
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.status)
	_, _ = w.Write(f.body)
	h.record(*meta, f.body, body, started)
}

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

	kind := "upstream_error"
	if st == http.StatusTooManyRequests {
		kind = "rate_limit_error"

		writeRetryAfter(w, f.err.retryAfter)
	}
	writeErr(w, st, kind, f.err.Error())
}

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

func cacheScope(model, identity, bodyHash string) string {
	if identity == "" {

		return model + "/" + anonymousScope + "/" + bodyHash
	}
	sum := sha256.Sum256([]byte(identity))
	return model + "/" + hex.EncodeToString(sum[:16]) + "/" + bodyHash
}

func (h *Handler) sessionKey(r *http.Request, alias string) string {
	if sid := r.Header.Get("X-Session-Id"); sid != "" {
		return "sess:" + sid + ":" + alias
	}
	token, _ := bearerToken(r)
	return "aff:" + token + ":" + alias
}

func baseBody(req map[string]any) []byte {
	b, err := json.Marshal(req)
	if err != nil {

		return []byte(`{}`)
	}
	return b
}

func upstreamBody(base []byte, upstreamModel string) []byte {
	return spliceTopLevelModel(base, upstreamModel)
}

func (h *Handler) maxAttempts(alias string) int {
	n := h.candidateCount(alias)
	if n < 2 {

		n = 2
	}
	if h.deps.Resilience == nil {
		return n
	}
	if rounds := h.effectiveRoundsRequest(alias); 1+rounds > n {
		n = 1 + rounds
	}
	if m := h.deps.Resilience.Retry.MaxRetryCredentials; m > 0 && n > m {
		n = m
	}
	if n < 1 {
		n = 1
	}
	return n
}

func (h *Handler) candidateCount(alias string) int {
	n := 0
	a, ok := h.deps.Registry.Alias(alias)
	if !ok {
		return 0
	}
	for _, t := range a.Targets {
		if p, ok := h.deps.Registry.Provider(t.Provider); ok {
			n += len(p.Keys)
		}
	}
	return n
}

func (h *Handler) effectiveRoundsRequest(alias string) int {
	if h.deps.Resilience == nil {
		return 0
	}
	def := h.deps.Resilience.Retry.RequestRetryDefault
	a, ok := h.deps.Registry.Alias(alias)
	if !ok || len(a.Targets) == 0 {
		return def
	}
	p, ok := h.deps.Registry.Provider(a.Targets[0].Provider)
	if !ok {
		return def
	}
	if len(p.Keys) > 0 {
		return resilience.EffectiveRounds(p.Keys[0].RequestRetry, p.RequestRetry, def)
	}
	return resilience.EffectiveRounds(nil, p.RequestRetry, def)
}

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

func costMicros(t model.Target, prompt, completion int) int64 {
	return money.CostMicros(t.PriceInputPerM, t.PriceOutputPerM, prompt, completion)
}

func spliceTopLevelModel(canonical []byte, upstreamModel string) []byte {
	enc, err := json.Marshal(upstreamModel)
	if err != nil {
		return canonical
	}
	needle := []byte(`"model":`)
	keyStart, valStart, valEnd := topLevelStringField(canonical, needle)
	if keyStart < 0 {

		return canonical
	}
	_ = keyStart
	out := make([]byte, 0, len(canonical)-(valEnd-valStart)+len(enc))
	out = append(out, canonical[:valStart]...)
	out = append(out, enc...)
	out = append(out, canonical[valEnd:]...)
	return out
}

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
