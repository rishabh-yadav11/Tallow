package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/rishabh-yadav11/tallow/internal/routing"
)

// forwardError distinguishes retryable, transport-level, and already-streamed
// failures so the retry loop can apply the exact first-token policy.
type forwardError struct {
	retryable     bool
	transport     bool
	streamStarted bool
	status        int
	msg           string
}

func (e *forwardError) Error() string { return e.msg }

// result carries token usage and (for non-stream) the response body.
type result struct {
	prompt     int
	completion int
	respBody   []byte
}

// forward sends one upstream request. Streaming passthrough never buffers the
// full body; non-streaming reads the full body before writing so a failure is
// always retried before the client sees anything.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, sel *routing.Selection, body []byte, stream bool) (result, *forwardError) {
	url := strings.TrimRight(sel.BaseURL, "/") + "/chat/completions"
	ctx := r.Context()
	var cancel context.CancelFunc
	if sel.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, sel.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return result{}, &forwardError{msg: "build upstream request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if sel.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+sel.Secret)
	}
	resp, err := h.deps.Client.Do(req)
	if err != nil {
		return result{}, &forwardError{retryable: true, transport: true, msg: "upstream: " + err.Error()}
	}
	defer resp.Body.Close()

	if stream {
		return h.streamForward(w, resp)
	}
	return h.plainForward(w, resp)
}

// plainForward buffers the whole response, returning it only on full success.
func (h *Handler) plainForward(w http.ResponseWriter, resp *http.Response) (result, *forwardError) {
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return result{}, &forwardError{retryable: true, msg: "read upstream: " + err.Error()}
	}
	if resp.StatusCode >= 400 {
		fe := &forwardError{status: resp.StatusCode, msg: upstreamErrorMsg(resp.StatusCode, b)}
		fe.retryable = isRetryableStatus(resp.StatusCode)
		return result{}, fe
	}
	prompt, completion := parseUsage(b)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(b)
	return result{prompt: prompt, completion: completion, respBody: b}, nil
}

// streamForward buffers only until the first token (first SSE data line),
// then passes through. Failure before the first token is a retryable error the
// client never sees; failure after is surfaced as an interruption.
func (h *Handler) streamForward(w http.ResponseWriter, resp *http.Response) (result, *forwardError) {
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		fe := &forwardError{status: resp.StatusCode, msg: upstreamErrorMsg(resp.StatusCode, b)}
		fe.retryable = isRetryableStatus(resp.StatusCode)
		return result{}, fe
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	br := bufio.NewReaderSize(resp.Body, 64*1024)
	var pre bytes.Buffer
	started := false
	var prompt, completion int

	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			if !started {
				// Failed before the first token: transparent retry.
				return result{}, &forwardError{retryable: true, msg: "stream failed before first token: " + err.Error()}
			}
			if err == io.EOF {
				return result{prompt: prompt, completion: completion}, nil
			}
			// Interrupted mid-stream: no seamless recovery by design.
			return result{prompt: prompt, completion: completion}, &forwardError{
				retryable: false, streamStarted: true, msg: "stream interrupted: " + err.Error(),
			}
		}

		if !started {
			pre.Write(line)
			if isDataLine(line) {
				started = true
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(pre.Bytes())
				pre.Reset()
				if flusher != nil {
					flusher.Flush()
				}
			}
			continue
		}

		_, _ = w.Write(line)
		if flusher != nil {
			flusher.Flush()
		}
		if p, c, ok := dataUsage(line); ok {
			prompt, completion = p, c
		}
	}
}

func isDataLine(line []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:"))
}

// dataUsage extracts usage tokens from an SSE data line when present.
func dataUsage(line []byte) (int, int, bool) {
	t := bytes.TrimSpace(line)
	if !bytes.HasPrefix(t, []byte("data:")) {
		return 0, 0, false
	}
	payload := bytes.TrimSpace(t[len("data:"):])
	if bytes.Equal(payload, []byte("[DONE]")) {
		return 0, 0, false
	}
	var v struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &v); err != nil {
		return 0, 0, false
	}
	return v.Usage.PromptTokens, v.Usage.CompletionTokens, true
}

func parseUsage(body []byte) (int, int) {
	var v struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return 0, 0
	}
	return v.Usage.PromptTokens, v.Usage.CompletionTokens
}

func isRetryableStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500
}

// upstreamErrorMsg extracts an OpenAI-style error message, else a status label.
func upstreamErrorMsg(status int, body []byte) string {
	var v struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &v) == nil && v.Error.Message != "" {
		return v.Error.Message
	}
	return http.StatusText(status)
}
