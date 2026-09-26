package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestH3UsageFieldsAreCoercedIndependently is the H3 regression.
//
// encoding/json aborts the ENTIRE struct decode on the first type mismatch for a
// typed field. The old code declared one typed `int` per field, so a single
// float- or string-encoded count discarded BOTH numbers, and the caller
// discarded the ok=false return, making the loss silent. Every case below is
// taken from the audit's measurements.
func TestH3UsageFieldsAreCoercedIndependently(t *testing.T) {
	cases := []struct {
		name           string
		body           string
		wantPrompt     int
		wantCompletion int
	}{
		{
			name:           "baseline ints",
			body:           `{"usage":{"prompt_tokens":1500,"completion_tokens":250}}`,
			wantPrompt:     1500,
			wantCompletion: 250,
		},
		{
			// The whole point: a float prompt count must not cost us the
			// completion count. Previously p=0 c=0.
			name:           "float prompt, int completion",
			body:           `{"usage":{"prompt_tokens":1500.0,"completion_tokens":250}}`,
			wantPrompt:     1500,
			wantCompletion: 250,
		},
		{
			name:           "both floats",
			body:           `{"usage":{"prompt_tokens":1500.0,"completion_tokens":250.0}}`,
			wantPrompt:     1500,
			wantCompletion: 250,
		},
		{
			// Previously p=0 c=0.
			name:           "numeric strings",
			body:           `{"usage":{"prompt_tokens":"1500","completion_tokens":"250"}}`,
			wantPrompt:     1500,
			wantCompletion: 250,
		},
		{
			// A null prompt must not take the completion count down with it.
			name:           "null prompt",
			body:           `{"usage":{"prompt_tokens":null,"completion_tokens":250}}`,
			wantPrompt:     0,
			wantCompletion: 250,
		},
		{
			name:           "null completion",
			body:           `{"usage":{"prompt_tokens":1500,"completion_tokens":null}}`,
			wantPrompt:     1500,
			wantCompletion: 0,
		},
		{
			// Anthropic-style spelling, which a strict OpenAI-only decode missed
			// entirely.
			name:           "input_tokens spelling",
			body:           `{"usage":{"input_tokens":1500,"completion_tokens":250}}`,
			wantPrompt:     1500,
			wantCompletion: 250,
		},
		{
			name:           "output_tokens spelling",
			body:           `{"usage":{"input_tokens":1500,"output_tokens":250}}`,
			wantPrompt:     1500,
			wantCompletion: 250,
		},
		{
			// vLLM / Ollama tokenizer accounting.
			name:           "vllm eval_count spelling",
			body:           `{"usage":{"prompt_eval_count":1500,"eval_count":250}}`,
			wantPrompt:     1500,
			wantCompletion: 250,
		},
		{
			// A garbage prompt value degrades that field only.
			name:           "non-numeric prompt string",
			body:           `{"usage":{"prompt_tokens":"not-a-number","completion_tokens":250}}`,
			wantPrompt:     0,
			wantCompletion: 250,
		},
		{
			// A negative count would subtract from usage totals downstream.
			name:           "negative prompt rejected",
			body:           `{"usage":{"prompt_tokens":-5,"completion_tokens":250}}`,
			wantPrompt:     0,
			wantCompletion: 250,
		},
		{
			// Absurd value must not overflow the cost multiplication.
			name:           "absurd count rejected",
			body:           `{"usage":{"prompt_tokens":1e30,"completion_tokens":250}}`,
			wantPrompt:     0,
			wantCompletion: 250,
		},
		{
			name:           "exponent notation accepted",
			body:           `{"usage":{"prompt_tokens":1.5e3,"completion_tokens":250}}`,
			wantPrompt:     1500,
			wantCompletion: 250,
		},
		{
			name:           "no usage block at all",
			body:           `{"id":"x","object":"chat.completion"}`,
			wantPrompt:     0,
			wantCompletion: 0,
		},
		{
			name:           "empty usage object",
			body:           `{"usage":{}}`,
			wantPrompt:     0,
			wantCompletion: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotP, gotC := parseUsage([]byte(tc.body))
			if gotP != tc.wantPrompt || gotC != tc.wantCompletion {
				t.Fatalf("parseUsage(%s) = p=%d c=%d, want p=%d c=%d (H3: one bad field must not discard both)",
					tc.body, gotP, gotC, tc.wantPrompt, tc.wantCompletion)
			}
		})
	}
}

// TestH3DataUsageSSEPathMatchesPlainPath checks the streaming path shares the
// fix. The audit noted the same all-or-nothing decode was duplicated at
// dataUsage, so fixing only one would have left streaming silently under-billed.
func TestH3DataUsageSSEPathMatchesPlainPath(t *testing.T) {
	line := []byte(`data: {"usage":{"prompt_tokens":1500.0,"completion_tokens":250}}`)
	p, c, ok := dataUsage(line)
	if !ok {
		t.Fatal("expected a usage-bearing data line to be recognised")
	}
	if p != 1500 || c != 250 {
		t.Fatalf("dataUsage = p=%d c=%d, want p=1500 c=250 (H3 on the SSE path)", p, c)
	}
}

// TestH3NonUsageLinesAreIgnored guards the sentinel paths so the new map-based
// decode did not start claiming lines that carry no usage.
func TestH3NonUsageLinesAreIgnored(t *testing.T) {
	for _, line := range []string{
		"data: [DONE]",
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		": keep-alive",
		"event: message",
		"data: not json at all",
		"data: [",
	} {
		if p, c, ok := dataUsage([]byte(line)); ok {
			t.Errorf("dataUsage(%q) claimed usage p=%d c=%d", line, p, c)
		}
	}
}

// TestM1UpstreamErrorTextIsNotRelayedToClient is the M1 regression. The audit
// captured a live gateway returning a provider body containing
// "invalid api key sk-live-AAAA1111 for org_zzz999" straight to the client.
func TestM1UpstreamErrorTextIsNotRelayedToClient(t *testing.T) {
	const secret = "sk-live-AAAA1111"
	const org = "org_zzz999"
	upstreamBody := []byte(`{"error":{"message":"invalid api key ` + secret + ` for ` + org + `","type":"invalid_request_error","code":"invalid_api_key"}}`)

	got := upstreamErrorMsg(http.StatusUnauthorized, upstreamBody)

	for _, leak := range []string{secret, org, "invalid api key", "invalid_api_key"} {
		if strings.Contains(got, leak) {
			t.Fatalf("client-facing error leaked upstream detail %q: %s (M1)", leak, got)
		}
	}
	// It must still say something useful and stable.
	if got != http.StatusText(http.StatusUnauthorized) {
		t.Fatalf("expected the status label, got %q", got)
	}
}

// TestM1DetailIsStillAvailableForLogging confirms the fix did not simply delete
// the provider's diagnostic, which is the single most useful thing an operator
// has when a request fails. upstreamErrorDetail is the logging path.
func TestM1DetailIsStillAvailableForLogging(t *testing.T) {
	upstreamBody := []byte(`{"error":{"message":"invalid api key sk-live-AAAA1111","type":"invalid_request_error","code":"invalid_api_key"}}`)
	detail := upstreamErrorDetail(upstreamBody)

	if !strings.Contains(detail, "sk-live-AAAA1111") {
		t.Fatalf("the logging path must retain the provider detail, got %q", detail)
	}
	if !strings.Contains(detail, "invalid_request_error") {
		t.Fatalf("expected the error type in the log detail, got %q", detail)
	}
	if !strings.Contains(detail, "invalid_api_key") {
		t.Fatalf("expected the error code in the log detail, got %q", detail)
	}
}

// TestM1MalformedUpstreamBodyStillYieldsAStatus covers the non-JSON upstream
// case, so a hostile or broken upstream returning HTML cannot push arbitrary
// text into the client-facing message.
func TestM1MalformedUpstreamBodyStillYieldsAStatus(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`<html><body>stack trace with sk-live-SECRET</body></html>`),
		[]byte("not json"),
		[]byte(""),
		[]byte(`{"error":"a string, not an object"}`),
		[]byte(`{"error":{"message":123}}`),
	} {
		got := upstreamErrorMsg(http.StatusBadGateway, body)
		if strings.Contains(got, "sk-live") {
			t.Fatalf("leaked upstream text from body %q: %s", body, got)
		}
		if got != http.StatusText(http.StatusBadGateway) {
			t.Fatalf("expected the status label for body %q, got %q", body, got)
		}
	}
}

// TestM2RedirectDoesNotResendProviderSecret is the M2 regression.
//
// The upstream 307-redirects to a second server. That second server must never
// observe the Authorization header, because Go preserves headers across a
// same-host 307 and re-sends them cross-host too.
func TestM2RedirectDoesNotResendProviderSecret(t *testing.T) {
	const providerSecret = "sk-live-LEAKME"

	var leaked bool
	var leakedValue string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			leaked = true
			leakedValue = got
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer attacker.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The first hop must genuinely carry the credential, otherwise the test
		// would pass simply because nothing sensitive was ever sent.
		if got := r.Header.Get("Authorization"); got != "Bearer "+providerSecret {
			t.Errorf("the first hop should carry the provider credential, got %q", got)
		}
		http.Redirect(w, r, attacker.URL+"/chat/completions", http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()

	client := newRefusingClient()
	req, err := http.NewRequest(http.MethodPost, upstream.URL+"/chat/completions", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+providerSecret)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if leaked {
		t.Fatalf("redirect target observed the provider credential %q (M2)", leakedValue)
	}
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("expected the 307 to be returned as-is, got %d", resp.StatusCode)
	}
}

// TestM2SameHostRedirectAlsoRefused covers the case the audit called out
// specifically: Go preserves headers on a SAME-HOST 307, so a "same host only"
// policy would still leak whenever that host resolves to an attacker. The rule
// enforced is therefore absolute, not host-scoped.
func TestM2SameHostRedirectAlsoRefused(t *testing.T) {
	var secondHopHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/first", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/second", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/second", func(w http.ResponseWriter, r *http.Request) {
		secondHopHits++
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := newRefusingClient()
	resp, err := client.Post(srv.URL+"/first", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if secondHopHits != 0 {
		t.Fatalf("the redirect target was contacted %d times; a same-host redirect must not be followed (M2)", secondHopHits)
	}
}

// TestM2NoRedirectMeansNormalTrafficIsUnaffected confirms the guard does not
// break the ordinary case: a direct 200 must still arrive intact.
func TestM2NoRedirectMeansNormalTrafficIsUnaffected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-live-OK" {
			t.Errorf("direct request lost its credential: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
	}))
	defer srv.Close()

	client := newRefusingClient()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/chat/completions", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-live-OK")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var v struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v.Usage.PromptTokens != 7 || v.Usage.CompletionTokens != 3 {
		t.Fatalf("body mangled: %+v", v.Usage)
	}
}

// newRefusingClient mirrors the production upstream client, which refuses
// redirects. The assertion that the PRODUCTION client is configured this way
// lives in internal/app (TestM2UpstreamClientRefusesRedirects); it cannot live
// here because app imports proxy and a test-only import back would be a cycle.
func newRefusingClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}
