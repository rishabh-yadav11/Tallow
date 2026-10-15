package proxy

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/compact"
	"github.com/rishabh-yadav11/tallow/internal/model"
)

func (h *Handler) responsesCompletion(w http.ResponseWriter, r *http.Request) {
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

	if items, ok := req["input"].([]any); ok {
		h.compactResponsesInput(items)
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

	base := baseBody(req)

	canon, cacheable := h.cacheKey(r, req, alias, stream)
	if cacheable {
		if e, hit := h.deps.Cache.Get(canon); hit {

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

		if f.leader {
			meta.PromptTokens = f.prompt
			meta.CompletionTokens = f.completion
			meta.CostMicros = f.costMicros
		} else {
			meta.Coalesced = true
			meta.RouteReason = "coalesced_" + f.sel.Reason
		}

		if f.leader {
			h.deps.Cache.Set(f.entry())
		}
		h.serveFetched(w, &meta, f, body, started)
		return
	}

	h.serveUncached(w, r, alias, sessionKey, base, body, stream, &meta, started)
}

func (h *Handler) compactResponsesInput(items []any) {
	cfg := h.deps.Compaction
	if !cfg.Enabled || cfg.MaxToolOutputChars <= 0 {
		return
	}
	var toCompact []any
	var sources []map[string]any
	for _, it := range items {
		obj, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if len(obj) == 0 {
			continue
		}

		if typ, _ := obj["type"].(string); typ == "function_call_output" {
			if out, ok := obj["output"].(string); ok {
				stage := map[string]any{"role": "tool", "content": out}
				toCompact = append(toCompact, stage)
				sources = append(sources, obj)
			}
			continue
		}

		if role, _ := obj["role"].(string); role == "tool" {
			toCompact = append(toCompact, obj)
			sources = append(sources, obj)
		}
	}
	compact.Apply(toCompact, cfg)
	for i, s := range sources {
		stage, _ := toCompact[i].(map[string]any)
		if stage == nil {
			continue
		}
		if content, ok := stage["content"].(string); ok {
			s["output"] = content
		}
	}
}
