package proxy

// Audit tests for the streamForward first-token contract:
//   - the pre-first-token buffer is bounded, so an upstream that sends only
//     non-data lines cannot pin the handler open or grow memory without bound
//     (item 1);
//   - EOF before the first token is a retryable failure that walks the 2-key
//     fallback chain and lands on 502, invisible to the client (item 2);
//   - a read error AT/AFTER the first token is an interruption (retryable=false,
//     no retry) after the client already received 200 + the first line (item 3);
//   - send() with an empty BaseURL errors instead of panicking, and an empty
//     Secret emits no Authorization header (item 13);
//   - the non-streaming read-error branches of readUpstreamBody and
//     plainForward are retryable (item 16).

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/registry"
	"github.com/rishabh-yadav11/tallow/internal/routing"
)

// streamScriptRoundTripper returns a crafted Response per call so a test can
// drive the full handler path (send -> streamForward) with a body that would be
// impossible to produce from a real upstream (endless non-data lines, EOF
// before data, a read error mid-stream).
type streamScriptRoundTripper struct {
	mu      sync.Mutex
	calls   int
	factory func(call int) *http.Response
}

func (t *streamScriptRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	return t.factory(t.calls), nil
}

func (t *streamScriptRoundTripper) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

// streamHandler wires a handler whose upstream is replaced by the scripted round
// tripper, so the test controls the exact byte stream streamForward sees.
func streamHandler(t *testing.T, reg *registry.Registry, rt http.RoundTripper) *Handler {
	t.Helper()
	return newHarness(t, Deps{
		Router:   newRouter(t, reg),
		Registry: reg,
		Client:   &http.Client{Transport: rt},
	})
}

// echoResponse builds a 200 streaming response with the given body.
func echoResponse(body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       body,
	}
}

// endlessNonData returns a reader that produces `: keep-alive\n` comment lines
// forever, with no data line ever.
type endlessNonData struct{}

func (endlessNonData) Read(p []byte) (int, error) {
	n := len(p)
	if n > len(": keep-alive\n") {
		n = len(": keep-alive\n")
	}
	return copy(p, ": keep-alive\n"[:n]), nil
}

// neverEmptyClose is a no-op closer over the above.
type neverEmptyClose struct{ endlessNonData }

func (neverEmptyClose) Close() error { return nil }

// firstLineThenError issues exactly one data line, then fails the next read.
// body[...] The error is non-EOF so streamForward must classify it as an
// interruption, not a clean end of stream.
type firstLineThenError struct{ sent bool }

func (r *firstLineThenError) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "data: {\"usage\":{\"prompt_tokens\":5}}\n"), nil
	}
	return 0, errors.New("boom: upstream socket reset")
}

func (r *firstLineThenError) Close() error { return nil }

func TestStreamForwardEndlessNonDataTerminatesBounded(t *testing.T) {
	// An upstream that answers 200 and then sends only `: keep-alive\n` comment
	// lines forever must not pin the handler open: the pre-first-token buffer is
	// bounded, and exceeding it terminates the stream with a retryable error.
	rt := &streamScriptRoundTripper{}
	rt.factory = func(int) *http.Response {
		return echoResponse(neverEmptyClose{})
	}
	h := streamHandler(t, oneAliasRegistry("http://scripted.invalid"), rt)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doPost(t, h, "", `{"model":"flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	}()

	select {
	case rec := <-done:
		// The stream never produced a token, and the failure is retryable, so the
		// chain is exhausted: the client must receive a 502 rather than a hang.
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("terminates with status %d, want 502: the client must get an "+
				"error instead of an open stream", rec.Code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("streamForward buffered non-data lines without bound: a 200 that " +
			"never emits a data line must terminate instead of hanging")
	}
}

func TestStreamForwardEOFBeforeFirstTokenRetriesAcrossKeys(t *testing.T) {
	// EOF before any data line is the retryable first-token failure. Two keys:
	// the first must be retried, the second also fails, so the client gets a 502
	// and the chain names both.
	rt := &streamScriptRoundTripper{}
	rt.factory = func(int) *http.Response {
		return echoResponse(io.NopCloser(strings.NewReader(": keep-alive\n")))
	}
	h := streamHandler(t, twoProviderRegistry(t), rt)

	rec := doPost(t, h, "", `{"model":"flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: an EOF before the first token is a "+
			"retryable failure and the exhausted chain is a gateway error", rec.Code)
	}
	if rt.count() != 2 {
		t.Fatalf("round trips = %d, want 2: the first-token failure must be "+
			"retried across the 2-key alias, reaching the second key", rt.count())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "after 2") {
		t.Errorf("exhausted message %q does not report the attempt count: the "+
			"client cannot tell the chain was actually walked", body)
	}
}

func TestStreamForwardMidStreamReadErrorStopsWithoutRetry(t *testing.T) {
	// A read error AFTER the first token is an interruption, not a retryable
	// failure. The client has already received 200 + the first line, and the
	// gateway must NOT retry (the bytes cannot be replayed).
	rt := &streamScriptRoundTripper{}
	rt.factory = func(int) *http.Response {
		return echoResponse(&firstLineThenError{})
	}
	h := streamHandler(t, oneAliasRegistry("http://scripted.invalid"), rt)

	rec := doPost(t, h, "", `{"model":"flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the client already received 200 + the "+
			"first line before the interruption", rec.Code)
	}
	if got := rec.Body.String(); !strings.HasPrefix(got, "data: ") {
		t.Fatalf("body %q does not carry the first data line the client saw "+
			"before the interruption", got)
	}
	rt.mu.Lock()
	calls := rt.calls
	rt.mu.Unlock()
	if calls != 1 {
		t.Fatalf("round trips = %d, want 1: an interruption after the first token "+
			"must not be retried, because the stream cannot be replayed", calls)
	}
}

func TestSendEmptyBaseURLErrorsNoPanic(t *testing.T) {
	// send with an empty BaseURL (reachable only if the router guard were ever
	// missed) must return a forwardError instead of panicking; the retry loop
	// maps an exhausted chain of these to 502.
	h := &Handler{deps: Deps{Client: &http.Client{}}}
	_, ferr := h.send(
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&routing.Selection{},
		nil,
	)
	if ferr == nil {
		t.Fatal("send with an empty BaseURL returned no error")
	}
}

func TestSendEmptySecretOmitsAuthorizationHeader(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	body := []byte(`{"model":"m"}`)
	h := &Handler{deps: Deps{Client: srv.Client()}}

	_, ferr := h.send(
		httptest.NewRequest(http.MethodPost, srv.URL+"/chat/completions", nil),
		&routing.Selection{BaseURL: srv.URL, Secret: ""}, body,
	)
	if ferr != nil {
		t.Fatalf("send with empty secret: %v", ferr)
	}
	if len(got) != 1 || got[0] != "" {
		t.Fatalf("empty Secret still produced Authorization header %q: the "+
			"gateway must not send a Bearer credential it does not have", got)
	}
}

func TestReadUpstreamBodyErrorIsRetryable(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(errReader{}),
	}
	if _, ferr := readUpstreamBody(resp); ferr == nil || !ferr.retryable {
		t.Fatalf("readUpstreamBody on a failing body: got %+v, want a retryable "+
			"error so the buffered path retries instead of serving a partial body",
			ferr)
	}
}

func TestPlainForwardReadErrorIsRetryable(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(errReader{}),
	}
	rec := httptest.NewRecorder()
	h := &Handler{}
	if _, ferr := h.plainForward(rec, resp); ferr == nil || !ferr.retryable {
		t.Fatalf("plainForward on a failing body: got %+v, want a retryable "+
			"error with nothing written", ferr)
	}
	if rec.Code != 0 && rec.Body.Len() != 0 {
		t.Fatalf("plainForward wrote a partial response before reading the whole "+
			"body (code=%d body=%q)", rec.Code, rec.Body.String())
	}
}

// errReader returns a transport-level error on every Read call, simulating a
// broken upstream connection.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read error") }

// Audit item 14: coerceTokenCount accepts an integer exactly at maxTokenCount,
// and rejects the next value (and anything past it). The cost multiplication is
// only safe if an absurd upstream count cannot pass the gate.
func TestCoerceTokenCountBoundaryAtMax(t *testing.T) {
	bad := []string{`1000000000001`, `1.000000000001e12`, `1000000000002`, `-1`}
	for _, s := range bad {
		if n, ok := coerceTokenCount(json.RawMessage(s)); ok {
			t.Fatalf("coerceTokenCount(%q) = %d, want rejected: it is past the "+
				"cost-arithmetic bound", s, n)
		}
	}
	good := []string{`1000000000000`, `1e12`, `1.0e12`, `"1000000000000"`, `999999999999`}
	for _, s := range good {
		if n, ok := coerceTokenCount(json.RawMessage(s)); !ok {
			t.Fatalf("coerceTokenCount(%q) rejected a value at or below the bound", s)
		} else if n < 0 || n > maxTokenCount {
			t.Fatalf("coerceTokenCount(%q) = %d, outside [0, maxTokenCount]", s, n)
		}
	}
}

// twoProviderRegistry builds two providers via separate round trips, each with
// one key and its own URL, so the failure chain walks alpha/k1 then beta/k2.
func twoProviderRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	return registry.New(
		[]model.Provider{
			{Name: "alpha", BaseURL: "http://alpha.invalid", Keys: []model.Key{{ID: "k1", Secret: "sk-a"}}},
			{Name: "beta", BaseURL: "http://beta.invalid", Keys: []model.Key{{ID: "k2", Secret: "sk-b"}}},
		},
		[]model.Alias{{
			Name: "flash",
			Targets: []model.Target{
				{Provider: "alpha", Model: "flash-v4"},
				{Provider: "beta", Model: "flash-v4"},
			},
		}},
	)
}
