package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/routing"
)

// responses implements the OpenAI Responses API v2 endpoint (POST /v1/responses).
//
// It translates the Responses request shape to the upstream chat-completions shape,
// routes through the same Router/Select path as chatCompletions, and translates the
// chat-completions response back to a Responses response. Streaming reuses the
// first-token contract from chat.go: nothing is sent before the first SSE data line,
// so a pre-first-token failure is retryable and invisible.
//
// This is deliberately not routed through forward(), because forward writes the
// chat-completions shape to the client; Responses needs the upstream bytes so it can
// round-trip the translation, so it forwards via send() and translates itself.
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
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

	messages, err := responsesToMessages(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	started := time.Now()
	meta := model.RequestMeta{ID: randID(), StartedAt: started, Model: alias, Stream: stream, Status: "ok"}
	sessionKey := h.sessionKey(r, alias)

	sel, err := h.deps.Router.Select(alias, sessionKey, nil)
	if err != nil {
		h.responsesRoutingFail(w, &meta, body, started, err)
		return
	}
	meta.Provider = sel.Provider
	meta.Key = sel.Key
	meta.UpstreamModel = sel.Model
	meta.RouteReason = sel.Reason

	chatReq := responsesUpstreamRequest(req, sel.Model, messages, stream)
	upBody, err := json.Marshal(chatReq)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "build upstream request")
		return
	}

	if stream {
		h.responsesStream(w, r, sel, upBody, &meta, body, started)
		return
	}
	h.responsesPlain(w, r, sel, upBody, &meta, body, started)
}

// responsesRoutingFail reports a Select failure the same way chatCompletions
// does: a limit-only failure is a 429, anything else a 502.
func (h *Handler) responsesRoutingFail(w http.ResponseWriter, meta *model.RequestMeta, body []byte, started time.Time, err error) {
	meta.Status = "error"
	meta.Err = err.Error()
	h.record(*meta, nil, body, started)
	if isLimitError(err) {
		writeErr(w, http.StatusTooManyRequests, "rate_limit_error", meta.Err)
		return
	}
	writeErr(w, http.StatusBadGateway, "upstream_error", meta.Err)
}

// isLimitError reports whether a routing error is purely limit-driven (a rate
// limit or exhausted quota), so the caller can answer 429 instead of 502. It
// mirrors the chat path's classification of routing.ErrNoRoute + Limited.
func isLimitError(err error) bool {
	if !errors.Is(err, routing.ErrNoRoute) {
		return false
	}
	var re *routing.RouteError
	return errors.As(err, &re) && re.Limited
}

// responsesPlain handles the non-streaming Responses request: forward, translate
// the chat-completions response back into a Responses object, and record.
func (h *Handler) responsesPlain(w http.ResponseWriter, r *http.Request, sel *routing.Selection, upBody []byte, meta *model.RequestMeta, reqBody []byte, started time.Time) {
	resp, ferr := h.send(r, sel, upBody)
	if ferr != nil {
		h.finishResponsesFailure(w, meta, sel, reqBody, started, ferr)
		return
	}
	defer resp.Body.Close()
	b, ferr := readUpstreamBody(resp)
	if ferr != nil {
		h.finishResponsesFailure(w, meta, sel, reqBody, started, ferr)
		return
	}
	prompt, completion := parseUsage(b)
	cost := costMicros(sel.Target, prompt, completion)
	out := translateResponse(b, sel.Model)
	// writeJSON sets the JSON content type and the DefaultRemoteMaxBytes-friendly
	// body. It is the same writer chatCompletions uses for errors.
	writeJSON(w, http.StatusOK, out)

	meta.Status = "ok"
	meta.PromptTokens = prompt
	meta.CompletionTokens = completion
	meta.CostMicros = cost
	h.deps.Router.RecordSuccess(sel)
	h.deps.Router.Release(sel, cost)
	h.record(*meta, b, reqBody, started)
}

// responsesUpstreamRequest builds the chat-completions body a Responses request
// maps to: messages from the Responses input, plus max_tokens and the stream
// flag. Called after routing so the upstream model is already selected.
func responsesUpstreamRequest(req map[string]any, upstreamModel string, messages []any, stream bool) map[string]any {
	out := map[string]any{
		"model":    upstreamModel,
		"messages": messages,
		"stream":   stream,
	}
	if mo, ok := req["max_output_tokens"]; ok {
		out["max_tokens"] = mo
	}
	// Responses exposes tool/function calling under `tools`; map it straight
	// through to the chat shape (both dialects share the tools array format).
	if tools, ok := req["tools"]; ok {
		out["tools"] = tools
	}
	if td, ok := req["tool_choice"]; ok {
		out["tool_choice"] = td
	}
	return out
}

// responsesToMessages converts a Responses `input` (string or array) plus
// `instructions` into a chat-completions `messages` array. `instructions` is a
// prepended system message. Each input element is normalised to a {role,
// content} message.
func responsesToMessages(req map[string]any) ([]any, error) {
	var messages []any
	if inst, ok := req["instructions"]; ok {
		if s, ok := inst.(string); ok && s != "" {
			messages = append(messages, map[string]any{"role": "system", "content": s})
		}
	}
	input, ok := req["input"]
	if !ok {
		return nil, fmt.Errorf("missing input")
	}
	switch v := input.(type) {
	case string:
		messages = append(messages, map[string]any{"role": "user", "content": v})
	case []any:
		for _, item := range v {
			msg, err := inputItemToMessage(item)
			if err != nil {
				return nil, err
			}
			messages = append(messages, msg)
		}
	default:
		return nil, fmt.Errorf("input must be a string or an array, got %T", input)
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("input produced no messages")
	}
	return messages, nil
}

// inputItemToMessage normalises one Responses input array element to a message.
// Accepted forms: a plain string, and an object with `role` + `content` where
// content is a string or an array of {type,text} parts (input_text backed).
func inputItemToMessage(item any) (any, error) {
	switch v := item.(type) {
	case string:
		return map[string]any{"role": "user", "content": v}, nil
	case map[string]any:
		role, _ := v["role"].(string)
		if role == "" {
			role = "user"
		}
		content, _ := v["content"]
		switch c := content.(type) {
		case string:
			return map[string]any{"role": role, "content": c}, nil
		case []any:
			var textParts []string
			for _, part := range c {
				p, ok := part.(map[string]any)
				if !ok {
					continue
				}
				typ, _ := p["type"].(string)
				if typ == "" {
					typ = "output_text"
				}
				if t, ok := p["text"].(string); ok && (typ == "input_text" || typ == "output_text") {
					textParts = append(textParts, t)
				}
			}
			return map[string]any{"role": role, "content": strings.Join(textParts, "\n")}, nil
		case nil:
			return map[string]any{"role": role, "content": ""}, nil
		}
		return nil, fmt.Errorf("unsupported input item content type %T", content)
	default:
		return nil, fmt.Errorf("unsupported input item type %T", item)
	}
}

// responsesStream handles the streaming Responses request. It forwards the chat
// request with stream=true, then translates each upstream SSE line into a
// Responses event. It buffers until the first data line so a pre-first-token
// failure is a retryable error the client never sees.
func (h *Handler) responsesStream(w http.ResponseWriter, r *http.Request, sel *routing.Selection, upBody []byte, meta *model.RequestMeta, reqBody []byte, started time.Time) {
	resp, ferr := h.send(r, sel, upBody)
	if ferr != nil {
		h.finishResponsesFailure(w, meta, sel, reqBody, started, ferr)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		fe := &forwardError{status: resp.StatusCode, resp: b, msg: upstreamErrorMsg(resp.StatusCode, b)}
		fe.retryable = isRetryableStatus(resp.StatusCode)
		h.finishResponsesFailure(w, meta, sel, reqBody, started, fe)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	br := bufio.NewReaderSize(resp.Body, 64*1024)
	var pre bytes.Buffer
	startedFlag := false
	var prompt, completion int

	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			if !startedFlag {
				h.finishResponsesFailure(w, meta, sel, reqBody, started, &forwardError{
					retryable: true,
					msg:       "stream failed before first token: " + err.Error(),
				})
				return
			}
			if err == io.EOF {
				h.writeResponsesCompleted(w, flusher, sel.Model)
				h.finishResponsesSuccess(w, meta, sel, reqBody, started, prompt, completion)
				return
			}
			// Interrupted mid-stream: report as a partial stream, no recovery.
			meta.Status = "error"
			meta.Err = "stream interrupted"
			h.record(*meta, nil, reqBody, started)
			h.deps.Router.Release(sel, 0)
			h.deps.Router.RecordFailure(sel)
			return
		}
		out, done := translateChatSSE(line)
		if !startedFlag {
			if out != nil {
				pre.Write(out)
			} else {
				// Keep preamble lines (comments/events) verbatim so a proxy that
				// expects event framing still sees them; they carry no content.
				pre.Write(line)
			}
			if out != nil || done {
				startedFlag = true
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(pre.Bytes())
				pre.Reset()
				if flusher != nil {
					flusher.Flush()
				}
			}
			if done {
				h.writeResponsesCompleted(w, flusher, sel.Model)
				h.finishResponsesSuccess(w, meta, sel, reqBody, started, prompt, completion)
				return
			}
			continue
		}
		if out != nil {
			_, _ = w.Write(out)
			if flusher != nil {
				flusher.Flush()
			}
			if p, c, ok := dataUsage(line); ok {
				prompt, completion = p, c
			}
		}
		if done {
			h.writeResponsesCompleted(w, flusher, sel.Model)
			h.finishResponsesSuccess(w, meta, sel, reqBody, started, prompt, completion)
			return
		}
	}
}

// writeResponsesCompleted emits the terminal response.completed event.
func (h *Handler) writeResponsesCompleted(w http.ResponseWriter, flusher http.Flusher, model string) {
	evt := map[string]any{"type": "response.completed", "response": map[string]any{"model": model}}
	if b, err := json.Marshal(evt); err == nil {
		_, _ = w.Write(append(append([]byte("data: "), b...), []byte("\n\n")...))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// translateChatSSE converts one chat-completions SSE data line into a Responses
// SSE event. It returns the translated bytes (nil when the line carries no
// streamable content, e.g. a comment or an empty delta) and done=true for the
// [DONE] terminator.
func translateChatSSE(line []byte) (out []byte, done bool) {
	t := bytes.TrimSpace(line)
	if !bytes.HasPrefix(t, []byte("data:")) {
		return nil, false
	}
	payload := bytes.TrimSpace(t[len("data:"):])
	if bytes.Equal(payload, []byte("[DONE]")) {
		return nil, true
	}
	var chunk struct {
		ID      string `json:"id"`
		Choices []struct {
			Delta struct {
				Content *string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(payload, &chunk) != nil || len(chunk.Choices) == 0 || chunk.Choices[0].Delta.Content == nil {
		return nil, false
	}
	evt := map[string]any{
		"type":    "response.output_text.delta",
		"item_id": "msg_" + randID()[:10],
		"delta":   *chunk.Choices[0].Delta.Content,
	}
	b, _ := json.Marshal(evt)
	return append(append([]byte("data: "), b...), []byte("\n\n")...), false
}

// finishResponsesFailure settles a failed upstream attempt: release the
// reservation, account the failure, record, and write the error (only valid
// when nothing has been streamed to the client yet).
func (h *Handler) finishResponsesFailure(w http.ResponseWriter, meta *model.RequestMeta, sel *routing.Selection, reqBody []byte, started time.Time, ferr *forwardError) {
	meta.Status = "error"
	meta.Err = ferr.Error()
	h.record(*meta, nil, reqBody, started)
	h.deps.Router.Release(sel, 0)
	h.deps.Router.RecordFailure(sel)
	st := ferr.status
	if st == 0 {
		st = http.StatusBadGateway
	}
	writeErr(w, st, "upstream_error", ferr.Error())
}

// finishResponsesSuccess settles a completed upstream attempt.
func (h *Handler) finishResponsesSuccess(w http.ResponseWriter, meta *model.RequestMeta, sel *routing.Selection, reqBody []byte, started time.Time, prompt, completion int) {
	meta.Status = "ok"
	meta.PromptTokens = prompt
	meta.CompletionTokens = completion
	meta.CostMicros = costMicros(sel.Target, prompt, completion)
	h.deps.Router.RecordSuccess(sel)
	h.deps.Router.Release(sel, meta.CostMicros)
	h.record(*meta, nil, reqBody, started)
}

// translateResponse converts a chat-completions JSON body into a Responses
// response object.
func translateResponse(body []byte, model string) map[string]any {
	var chat struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(body, &chat)
	content := ""
	if len(chat.Choices) > 0 {
		switch c := chat.Choices[0].Message.Content.(type) {
		case string:
			content = c
		case []any:
			var parts []string
			for _, p := range c {
				if m, ok := p.(map[string]any); ok {
					if t, ok := m["text"].(string); ok {
						parts = append(parts, t)
					}
				}
			}
			content = strings.Join(parts, "")
		}
	}
	prompt, completion := 0, 0
	if chat.Usage != nil {
		prompt = chat.Usage.PromptTokens
		completion = chat.Usage.CompletionTokens
	}
	output := []any{}
	if content != "" {
		output = append(output, map[string]any{
			"type":    "message",
			"id":      "msg_" + randID()[:12],
			"status":  "completed",
			"role":    "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": content, "annotations": []any{}}},
		})
	}
	return map[string]any{
		"id":      chat.ID,
		"object":  "response",
		"created": time.Now().Unix(),
		"model":   model,
		"status":  "completed",
		"output":  output,
		"usage": map[string]any{
			"input_tokens":  prompt,
			"output_tokens": completion,
			"total_tokens":  prompt + completion,
		},
	}
}
