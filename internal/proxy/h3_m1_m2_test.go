package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

			name:           "numeric strings",
			body:           `{"usage":{"prompt_tokens":"1500","completion_tokens":"250"}}`,
			wantPrompt:     1500,
			wantCompletion: 250,
		},
		{

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

			name:           "vllm eval_count spelling",
			body:           `{"usage":{"prompt_eval_count":1500,"eval_count":250}}`,
			wantPrompt:     1500,
			wantCompletion: 250,
		},
		{

			name:           "non-numeric prompt string",
			body:           `{"usage":{"prompt_tokens":"not-a-number","completion_tokens":250}}`,
			wantPrompt:     0,
			wantCompletion: 250,
		},
		{

			name:           "negative prompt rejected",
			body:           `{"usage":{"prompt_tokens":-5,"completion_tokens":250}}`,
			wantPrompt:     0,
			wantCompletion: 250,
		},
		{

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

	if got != http.StatusText(http.StatusUnauthorized) {
		t.Fatalf("expected the status label, got %q", got)
	}
}

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

		if got := r.Header.Get("Authorization"); got != "Bearer "+providerSecret {
			t.Errorf("the first hop should carry the provider credential, got %q", got)
		}
		http.Redirect(w, r, attacker.URL+"/responses", http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()

	client := newRefusingClient()
	req, err := http.NewRequest(http.MethodPost, upstream.URL+"/responses", strings.NewReader(`{"model":"m"}`))
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
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/responses", strings.NewReader(`{}`))
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

func newRefusingClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}
