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

	"github.com/rishabh-yadav11/tallow/internal/routing"
)

// forwardError distinguishes retryable, transport-level, and already-streamed
// failures so the retry loop can apply the exact first-token policy.
//
// msg is CLIENT-SAFE. It reaches response bodies and audit rows, so it must
// never carry the upstream base_url, a host, a port, or a credential. Errors
// that cannot satisfy that must put their detail in private instead, which is
// logged and never returned.
type forwardError struct {
	retryable     bool
	transport     bool
	streamStarted bool
	status        int
	msg           string
	private       string
}

func (e *forwardError) Error() string { return e.msg }

// logDetail returns the full error text for logging, including anything the
// client-safe msg withheld.
func (e *forwardError) logDetail() string {
	if e.private == "" {
		return e.msg
	}
	return e.private
}

// result carries token usage and (for non-stream) the response body.
type result struct {
	prompt     int
	completion int
	respBody   []byte
	status     int
}

// send performs the upstream HTTP request and returns the raw response.
//
// Split out from forward so the buffered cacheable path can issue the same
// request WITHOUT writing to a client ResponseWriter, which is what makes an
// upstream response safe to share between coalesced callers (H8).
func (h *Handler) send(r *http.Request, sel *routing.Selection, body []byte) (*http.Response, *forwardError) {
	url := strings.TrimRight(sel.BaseURL, "/") + "/chat/completions"
	ctx := r.Context()
	var cancel context.CancelFunc
	if sel.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, sel.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, &forwardError{msg: "build upstream request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if sel.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+sel.Secret)
	}
	resp, err := h.deps.Client.Do(req)
	if err != nil {
		return nil, &forwardError{
			retryable: true,
			transport: true,
			// Only the category, never the error. err is a *url.Error, which
			// renders as `Post "<full base_url>": <cause>`, and its cause
			// repeats the address too: net.OpError prefixes "dial tcp <addr>:".
			// So neither the whole string nor a stripped prefix of it is safe
			// to return. The full text goes to private for the log, and the
			// client learns only that the connection failed, which is all it
			// can act on.
			msg:     "connection failed",
			private: "upstream: " + err.Error(),
		}
	}
	return resp, nil
}

// readUpstreamBody reads a non-streaming upstream response and applies the
// status policy, without writing anything to a client.
func readUpstreamBody(resp *http.Response) ([]byte, *forwardError) {
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &forwardError{retryable: true, msg: "read upstream: " + err.Error()}
	}
	if resp.StatusCode >= 400 {
		fe := &forwardError{status: resp.StatusCode, msg: upstreamErrorMsg(resp.StatusCode, b)}
		fe.retryable = isRetryableStatus(resp.StatusCode)
		return nil, fe
	}
	return b, nil
}

// forward sends one upstream request. Streaming passthrough never buffers the
// full body; non-streaming reads the full body before writing so a failure is
// always retried before the client sees anything.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, sel *routing.Selection, body []byte, stream bool) (result, *forwardError) {
	resp, ferr := h.send(r, sel, body)
	if ferr != nil {
		return result{}, ferr
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
	return usageTokens(payload)
}

func parseUsage(body []byte) (int, int) {
	p, c, _ := usageTokens(body)
	return p, c
}

// usageTokens extracts prompt and completion token counts from a response body
// or SSE payload, coercing each field INDEPENDENTLY.
//
// H3: the previous version declared a single typed `int` per field inside one
// all-or-nothing json.Unmarshal. encoding/json aborts the ENTIRE struct decode
// on the first type mismatch for a typed field - it does not skip it the way it
// does for map[string]any - so a single float- or string-encoded token count
// discarded BOTH numbers. Measured against the real functions:
//
//	{"usage":{"prompt_tokens":1500,"completion_tokens":250}}        -> p=1500 c=250
//	{"usage":{"prompt_tokens":1500.0,"completion_tokens":250.0}}      -> p=0    c=0
//	{"usage":{"prompt_tokens":"1500","completion_tokens":"250"}}      -> p=0    c=0
//	{"usage":{"prompt_tokens":null,"completion_tokens":250}}          -> p=0    c=250
//	{"usage":{"input_tokens":1500,"completion_tokens":250}}           -> p=0    c=0
//
// Float and string encodings are emitted by several OpenAI-compatible gateways
// and by vLLM's older tokenizer accounting, and `input_tokens` is the
// Anthropic-style spelling. The caller discarded the ok=false return, so the
// loss was silent and compounded C3: cost recorded as zero with no error.
//
// Decoding into json.RawMessage and coercing per field means a bad
// prompt_tokens degrades to 0 while completion_tokens survives, and a field
// encoded as a float or a numeric string is accepted rather than discarded.
func usageTokens(payload []byte) (prompt, completion int, ok bool) {
	var v struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &v); err != nil || v.Usage == nil {
		return 0, 0, false
	}
	// Accept every spelling seen in the wild, not just the OpenAI one, since the
	// gateway fronts several provider dialects and a missing count is silent
	// under-billing either way.
	prompt = firstTokenCount(v.Usage, "prompt_tokens", "input_tokens", "prompt_eval_count")
	completion = firstTokenCount(v.Usage, "completion_tokens", "output_tokens", "eval_count")
	return prompt, completion, true
}

// firstTokenCount returns the first parseable token count among the given keys.
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

// coerceTokenCount parses one token-count field, accepting the encodings that
// real providers actually emit.
//
// A negative count is rejected rather than clamped: there is no arithmetic in
// which a negative token count is meaningful, and admitting one would let a
// hostile or buggy upstream subtract from the gateway's usage totals. A count
// so large it cannot be a real request is likewise rejected, because
// costMicros is derived from it by multiplication and an absurd value would
// overflow the budget arithmetic rather than merely misreport.
func coerceTokenCount(raw json.RawMessage) (int, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, false
	}
	// Strip surrounding quotes so a numeric string ("250") parses like a number.
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return 0, false
		}
		s = strings.TrimSpace(str)
	}
	// Accept "1500" and "1500.0" and "1.5e3" alike. A float decode is required
	// because encoding/json refuses an integer literal into a float field only
	// when it has a fractional part; parsing as float and rounding accepts
	// every case uniformly.
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	// Round rather than truncate: providers that emit 1500.0 mean 1500 tokens,
	// and truncating 1500.9 to 1500 is fine, but truncating a value that arrived
	// as 1500.999999 due to float accumulation should not lose a whole token.
	n := int(math.Round(f))
	if n < 0 || n > maxTokenCount {
		return 0, false
	}
	return n, true
}

// maxTokenCount bounds an accepted token count. 1e12 is far beyond any real
// request and keeps the downstream cost multiplication well inside int64.
const maxTokenCount = 1e12

func isRetryableStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500
}

// upstreamErrorMsg builds the client-facing error for an upstream failure.
//
// M1: this used to return the upstream's own error text verbatim, so a provider
// response of "invalid api key sk-live-AAAA1111 for org_zzz999" reached the
// client with the credential prefix and the account id intact. The gateway was
// functioning as an error-message relay for its own upstream providers'
// internals.
//
// The detail is not discarded - it is the single most useful thing an operator
// has when a request fails - so it is logged at ERROR and the client receives a
// stable generic message plus the upstream status. A caller can correlate
// through the gateway log rather than through a credential disclosure.
func upstreamErrorMsg(status int, body []byte) string {
	detail := upstreamErrorDetail(body)
	if detail != "" {
		// Logged, not returned. slog is used rather than a package-level logger
		// so this composes with whatever handler the app installed.
		slog.Error("upstream request failed",
			"status", status,
			"detail", detail)
	}
	return http.StatusText(status)
}

// upstreamErrorDetail extracts an OpenAI-style error message for LOGGING only.
// Never return this to a client; see upstreamErrorMsg.
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
