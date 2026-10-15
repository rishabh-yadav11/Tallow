package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/config"
	"github.com/rishabh-yadav11/tallow/internal/routing"
)

type forwardError struct {
	retryable     bool
	transport     bool
	streamStarted bool
	status        int
	msg           string
	private       string

	body []byte

	retryAfter time.Duration
}

func (e *forwardError) Error() string { return e.msg }

func (e *forwardError) logDetail() string {
	if e.private == "" {
		return e.msg
	}
	return e.private
}

type result struct {
	prompt     int
	completion int
	respBody   []byte
	status     int
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

const upstreamResponsesPath = "/responses"

func (h *Handler) send(r *http.Request, sel *routing.Selection, body []byte) (*http.Response, *forwardError) {
	url := strings.TrimRight(sel.BaseURL, "/") + upstreamResponsesPath
	ctx := r.Context()
	var cancel context.CancelFunc
	if sel.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, sel.Timeout)

	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, &forwardError{msg: "build upstream request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if sel.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+sel.Secret)
	}
	if ferr := proxyTargetGuard(h.deps.Client, req); ferr != nil {
		if cancel != nil {
			cancel()
		}
		return nil, ferr
	}
	resp, err := h.deps.Client.Do(req)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, &forwardError{
			retryable: true,
			transport: true,

			msg:     "connection failed",
			private: "upstream: " + err.Error(),
		}
	}
	if cancel != nil {
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	}
	return resp, nil
}

func proxyTargetGuard(client *http.Client, req *http.Request) *forwardError {
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		return nil
	}
	proxyURL, err := tr.Proxy(req)
	if err != nil {
		return &forwardError{transport: true, msg: "connection failed", private: "proxy lookup: " + err.Error()}
	}
	if proxyURL == nil {
		return nil
	}
	if err := config.CheckTargetPublic(req.URL.Hostname()); err != nil {
		return &forwardError{
			transport: true,

			msg:     "connection failed",
			private: "upstream proxy target: " + err.Error(),
		}
	}
	return nil
}

func readUpstreamBody(resp *http.Response) ([]byte, *forwardError) {
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &forwardError{retryable: true, msg: "read upstream: " + err.Error()}
	}
	if resp.StatusCode >= 400 {
		fe := &forwardError{status: resp.StatusCode, msg: upstreamErrorMsg(resp.StatusCode, b), body: b}
		fe.retryable = isRetryableStatus(resp.StatusCode)
		return nil, fe
	}
	return b, nil
}

func (h *Handler) forward(w http.ResponseWriter, r *http.Request, sel *routing.Selection, body []byte, stream bool) (result, *forwardError) {
	resp, ferr := h.send(r, sel, body)
	if ferr != nil {
		return result{}, ferr
	}
	defer resp.Body.Close()

	forced := ""
	if sel.Target.ForceMapping && sel.Alias != "" {
		forced = sel.Alias
	}

	if stream {
		return h.streamForward(w, resp, forced)
	}
	return h.plainForward(w, resp, forced)
}

func (h *Handler) plainForward(w http.ResponseWriter, resp *http.Response, forced string) (result, *forwardError) {
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return result{}, &forwardError{retryable: true, msg: "read upstream: " + err.Error()}
	}
	if resp.StatusCode >= 400 {
		fe := &forwardError{status: resp.StatusCode, msg: upstreamErrorMsg(resp.StatusCode, b), body: b}
		fe.retryable = isRetryableStatus(resp.StatusCode)
		return result{}, fe
	}
	if forced != "" {
		b = spliceTopLevelModel(b, forced)
	}
	prompt, completion := parseUsage(b)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(b)
	return result{prompt: prompt, completion: completion, respBody: b}, nil
}

const streamPreHeaderLimit = 256 << 10

func (h *Handler) streamForward(w http.ResponseWriter, resp *http.Response, forced string) (result, *forwardError) {
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		fe := &forwardError{status: resp.StatusCode, msg: upstreamErrorMsg(resp.StatusCode, b), body: b}
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

	done := make(chan struct{})
	defer close(done)
	type lineEvent struct {
		line []byte
		err  error
	}
	lines := make(chan lineEvent, 16)
	go func() {
		defer close(lines)
		for {
			line, err := br.ReadBytes('\n')
			if err != nil {
				select {
				case lines <- lineEvent{line: line, err: err}:
				case <-done:
				}
				return
			}
			select {
			case lines <- lineEvent{line: line}:
			case <-done:
				return
			}
		}
	}()

	var keepalive *time.Ticker
	var keepaliveC <-chan time.Time
	if h.deps.Keepalive > 0 {
		keepalive = time.NewTicker(h.deps.Keepalive)
		defer keepalive.Stop()
		keepaliveC = keepalive.C
	}

	for {
		select {
		case <-keepaliveC:
			if started {
				_, _ = w.Write([]byte(": keepalive\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
			continue
		case ev, ok := <-lines:
			if !ok {

				if !started {
					return result{}, &forwardError{retryable: true, msg: "stream failed before first token: EOF"}
				}
				return result{prompt: prompt, completion: completion}, nil
			}
			line, err := ev.line, ev.err
			if err != nil {
				if !started {

					return result{}, &forwardError{retryable: true, msg: "stream failed before first token: " + err.Error()}
				}
				if err == io.EOF {
					return result{prompt: prompt, completion: completion}, nil
				}

				return result{prompt: prompt, completion: completion}, &forwardError{
					retryable: false, streamStarted: true, msg: "stream interrupted: " + err.Error(),
				}
			}

			if !started {
				if pre.Len()+len(line) > streamPreHeaderLimit {

					return result{}, &forwardError{
						retryable: true,
						msg:       "stream failed before first token: no data line within " + strconv.Itoa(streamPreHeaderLimit/1024) + "KiB of preamble",
					}
				}
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

			out := line
			if forced != "" {
				out = patchStreamModel(line, forced)
			}
			_, _ = w.Write(out)
			if flusher != nil {
				flusher.Flush()
			}
			if p, c, ok := dataUsage(line); ok {
				prompt, completion = p, c
			}
		}
	}
}

func patchStreamModel(line []byte, alias string) []byte {
	t := bytes.TrimSpace(line)
	if !bytes.HasPrefix(t, []byte("data:")) {
		return line
	}
	payload := bytes.TrimSpace(bytes.TrimSuffix(t[len("data:"):], []byte("\r")))

	if len(payload) == 0 || (payload[0] != '{' && payload[0] != '[') {
		return line
	}
	patched := spliceTopLevelModel(payload, alias)
	if len(patched) == len(payload) && bytes.Equal(patched, payload) {
		return line
	}
	out := make([]byte, 0, len(line))
	out = append(out, t[:len(t)-len(payload)]...)
	out = append(out, patched...)
	out = append(out, line[len(t):]...)
	return out
}

func isDataLine(line []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:"))
}

func dataUsage(line []byte) (int, int, bool) {
	t := bytes.TrimSpace(line)
	if !bytes.HasPrefix(t, []byte("data:")) {
		return 0, 0, false
	}
	payload := bytes.TrimSpace(t[len("data:"):])
	if bytes.Equal(payload, []byte("[DONE]")) {
		return 0, 0, false
	}
	return usageTokens(payload)
}

func parseUsage(body []byte) (int, int) {
	p, c, _ := usageTokens(body)
	return p, c
}

func usageTokens(payload []byte) (prompt, completion int, ok bool) {
	var v struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &v); err != nil || v.Usage == nil {
		return 0, 0, false
	}

	prompt = firstTokenCount(v.Usage, "prompt_tokens", "input_tokens", "prompt_eval_count")
	completion = firstTokenCount(v.Usage, "completion_tokens", "output_tokens", "eval_count")
	return prompt, completion, true
}

func firstTokenCount(usage map[string]json.RawMessage, keys ...string) int {
	for _, k := range keys {
		raw, ok := usage[k]
		if !ok {
			continue
		}
		if n, ok := coerceTokenCount(raw); ok {
			return n
		}
	}
	return 0
}

func coerceTokenCount(raw json.RawMessage) (int, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, false
	}

	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return 0, false
		}
		s = strings.TrimSpace(str)
	}

	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}

	n := int(math.Round(f))
	if n < 0 || n > maxTokenCount {
		return 0, false
	}
	return n, true
}

const maxTokenCount = 1e12

func isRetryableStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500
}

func upstreamErrorMsg(status int, body []byte) string {
	detail := upstreamErrorDetail(body)
	if detail != "" {

		slog.Error("upstream request failed",
			"status", status,
			"detail", detail)
	}
	return http.StatusText(status)
}

func upstreamErrorDetail(body []byte) string {
	var v struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if v.Error.Message != "" {
		parts = append(parts, v.Error.Message)
	}
	if v.Error.Type != "" {
		parts = append(parts, "type="+v.Error.Type)
	}
	if v.Error.Code != nil {
		parts = append(parts, fmt.Sprintf("code=%v", v.Error.Code))
	}
	return strings.Join(parts, " ")
}
