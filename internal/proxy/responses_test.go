package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/store"
)

// TestResponsesRecordsThroughRetentionStore asserts that a non-streaming
// Responses request is persisted through the same retention store as chat
// completions (spec §1: "response recorded through the same retention store"),
// with the alias and the selected upstream model attributed on the row.
func TestResponsesRecordsThroughRetentionStore(t *testing.T) {
	up := newCountUpstream(t, 0)
	reg := oneAliasRegistry(up.srv.URL)
	st, err := store.Open(store.StoreConfig{Path: filepath.Join(t.TempDir(), "tallow.db")})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	h := newHarness(t, Deps{Router: newRouter(t, reg), Registry: reg, Store: st})
	rec := doResponsesPost(t, h, "", `{"model":"flash","input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	st.Flush()
	rows, err := st.RecentRequests(10)
	if err != nil {
		t.Fatalf("RecentRequests: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("Responses request produced no store row")
	}
	row := rows[0]
	if row.Model != "flash" {
		t.Fatalf("row model = %q, want the alias 'flash'", row.Model)
	}
	if row.UpstreamModel != "flash-v4" {
		t.Fatalf("row upstream_model = %q, want 'flash-v4'", row.UpstreamModel)
	}
	if row.Provider != "p1" || row.Key == "" {
		t.Fatalf("row not attributed to a route: %+v", row)
	}
}

// doResponsesPost posts a Responses request to the /v1/responses route.
func doResponsesPost(t *testing.T, h *Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec
}

func TestResponsesToMessages(t *testing.T) {
	req := map[string]any{
		"instructions": "Be terse.",
		"input": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "hi"},
			}},
			"second turn",
		},
	}
	msgs, err := responsesToMessages(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (system + 2 user), got %d", len(msgs))
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "Be terse." {
		t.Fatalf("system message: %+v", sys)
	}
	first := msgs[1].(map[string]any)
	if first["content"] != "hi" {
		t.Fatalf("input_text part: %+v", first)
	}
}

// TestResponsesRoutesAndTranslates exercises the full non-streaming path against
// a real upstream: the Responses input should reach the upstream as chat
// messages, and the chat response should come back shaped like a Responses
// object.
func TestResponsesRoutesAndTranslates(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if m, _ := body["messages"]; m == nil {
			t.Errorf("upstream received no messages: %s", raw)
		}
		if mt, _ := body["max_tokens"]; mt == nil {
			t.Errorf("max_output_tokens was not mapped to max_tokens: %s", raw)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chat_1","model":"flash-v4","choices":[{"message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
	}))
	defer upstream.Close()

	reg := oneAliasRegistry(upstream.URL)
	h := newHarness(t, Deps{
		Router:        newRouter(t, reg),
		Registry:      reg,
		MaxConcurrent: 8,
	})

	rec := doResponsesPost(t, h, "", `{"model":"flash","instructions":"Talk like a duck.","input":"quack","max_output_tokens":50}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["object"] != "response" {
		t.Fatalf("expected object=response, got %v", out["object"])
	}
	output, _ := out["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("expected one output item, got %+v", out["output"])
	}
	msg := output[0].(map[string]any)
	if msg["role"] != "assistant" {
		t.Fatalf("output role: %+v", msg)
	}
}

// TestResponsesStreamEmitsEvents drives the streaming path with an upstream
// that emits chat SSE chunks and verifies the translated Responses events.
func TestResponsesStreamEmitsEvents(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"a","choices":[{"delta":{"role":"assistant"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"a","choices":[{"delta":{"content":"hel"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"a","choices":[{"delta":{"content":"lo"}}],"usage":{"prompt_tokens":2,"completion_tokens":2}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	reg := oneAliasRegistry(upstream.URL)
	h := newHarness(t, Deps{Router: newRouter(t, reg), Registry: reg, MaxConcurrent: 8})

	rec := doResponsesPost(t, h, "", `{"model":"flash","input":"hi","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "response.output_text.delta") {
		t.Fatalf("expected output_text.delta events, got:\n%s", body)
	}
	if !strings.Contains(body, "response.completed") {
		t.Fatalf("expected response.completed terminator, got:\n%s", body)
	}
}

// TestResponsesStreamingSSE is a focused unit test of the SSE translator.
func TestTranslateChatSSE(t *testing.T) {
	out, done := translateChatSSE([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n"))
	if done {
		t.Fatal("content chunk must not be done")
	}
	if !strings.Contains(string(out), "response.output_text.delta") {
		t.Fatalf("delta line: %s", out)
	}
	if _, done := translateChatSSE([]byte("data: [DONE]\n")); !done {
		t.Fatal("expected [DONE] to set done")
	}
}
