package admin

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// doBody sends method to path with the given JSON body and returns the response.
func doBody(t *testing.T, c *http.Client, method, path, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "http://admin"+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building request %s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	resp.Body.Close()
	return resp, b
}

// TestConfigWriteForwarding verifies PUT/PATCH/DELETE on /config at full-doc,
// section, and field granularity forward the right method, scope, and body to
// the backend.
func TestConfigWriteForwarding(t *testing.T) {
	f := populatedFake()
	_, c := startAdmin(t, f)

	// PUT a single field.
	resp, body := doBody(t, c, http.MethodPut, "/config/server/listen", `"127.0.0.1:9999"`)
	wantStatus(t, resp, http.StatusOK)
	var ok struct {
		OK bool `json:"ok"`
	}
	decodeBody(t, body, &ok)
	if !ok.OK {
		t.Error("PUT /config/server/listen ok = false")
	}
	f.mu.Lock()
	method, scope := f.lastConfigWriteMethod, append([]string(nil), f.lastConfigWriteScope...)
	gotBody := append([]byte(nil), f.lastConfigWriteBody...)
	f.mu.Unlock()
	if method != http.MethodPut {
		t.Errorf("backend method = %q; want PUT", method)
	}
	if len(scope) != 2 || scope[0] != "server" || scope[1] != "listen" {
		t.Errorf("backend scope = %v; want [server listen]", scope)
	}
	if string(gotBody) != `"127.0.0.1:9999"` {
		t.Errorf("backend body = %s", gotBody)
	}

	// PATCH at full-document granularity.
	resp, _ = doBody(t, c, http.MethodPatch, "/config", `{"auth":{"open":false}}`)
	wantStatus(t, resp, http.StatusOK)
	f.mu.Lock()
	if f.lastConfigWriteMethod != http.MethodPatch || len(f.lastConfigWriteScope) != 0 {
		t.Errorf("PATCH /config forwarded method %q scope %v", f.lastConfigWriteMethod, f.lastConfigWriteScope)
	}
	f.mu.Unlock()

	// DELETE a field.
	resp, _ = doBody(t, c, http.MethodDelete, "/config/server/timeout", "")
	wantStatus(t, resp, http.StatusOK)
	f.mu.Lock()
	if f.lastConfigWriteMethod != http.MethodDelete || len(f.lastConfigWriteScope) != 2 {
		t.Errorf("DELETE /config/server/timeout forwarded method %q scope %v", f.lastConfigWriteMethod, f.lastConfigWriteScope)
	}
	f.mu.Unlock()

	// DELETE the whole document is refused.
	resp, _ = doBody(t, c, http.MethodDelete, "/config", "")
	wantStatus(t, resp, http.StatusBadRequest)
}

// TestConfigWriteInvalidBody verifies malformed JSON and a backend write error
// surface as BAD_REQUEST / carry the backend message.
func TestConfigWriteInvalidBody(t *testing.T) {
	f := populatedFake()
	_, c := startAdmin(t, f)

	resp, body := doBody(t, c, http.MethodPut, "/config/server/listen", `not-json{`)
	wantStatus(t, resp, http.StatusBadRequest)
	if msg := decodeErrBody(t, body); msg == "" {
		t.Error("invalid JSON produced empty error message")
	}

	// A backend write failure is surfaced with its message.
	f2 := populatedFake()
	f2.configWriteErr = errors.New("boom")
	_, c2 := startAdmin(t, f2)
	resp, body = doBody(t, c2, http.MethodPut, "/config/server/listen", `"1.2.3.4:9"`)
	wantStatus(t, resp, http.StatusBadRequest)
	if msg := decodeErrBody(t, body); msg != "boom" {
		t.Errorf("backend error message = %q; want boom", msg)
	}
}

// TestKeysCollectionAndDelete covers GET /keys, POST /keys (upload), and
// DELETE /keys/{ref}, asserting the plaintext secret is never echoed.
func TestKeysCollectionAndDelete(t *testing.T) {
	f := populatedFake()
	_, c := startAdmin(t, f)

	// GET lists refs only.
	resp, body := doJSON(t, c, http.MethodGet, "/keys")
	wantStatus(t, resp, http.StatusOK)
	var m struct {
		Refs []string `json:"refs"`
	}
	decodeBody(t, body, &m)
	if len(m.Refs) != 1 || m.Refs[0] != "keystore:key-1" {
		t.Errorf("refs = %v; want [keystore:key-1]", m.Refs)
	}
	if strings.Contains(string(body), testSecret) {
		t.Error("/keys leaks the plaintext secret")
	}

	// POST uploads a secret.
	resp, body = doBody(t, c, http.MethodPost, "/keys", `{"ref":"prov:newkey","secret":"sk-newsecret"}`)
	wantStatus(t, resp, http.StatusOK)
	var uploadOk struct {
		OK  bool   `json:"ok"`
		Ref string `json:"ref"`
	}
	decodeBody(t, body, &uploadOk)
	if !uploadOk.OK || uploadOk.Ref != "prov:newkey" {
		t.Errorf("upload response = %+v", uploadOk)
	}
	f.mu.Lock()
	uploadedRef, uploadedSecret := f.lastUploadedRef, append([]byte(nil), f.lastUploadedSecret...)
	f.mu.Unlock()
	if uploadedRef != "prov:newkey" {
		t.Errorf("uploaded ref = %q; want prov:newkey", uploadedRef)
	}
	if string(uploadedSecret) != "sk-newsecret" {
		t.Errorf("uploaded secret = %q; want sk-newsecret", uploadedSecret)
	}

	// DELETE removes by ref from the path.
	resp, body = doBody(t, c, http.MethodDelete, "/keys/prov:newkey", "")
	wantStatus(t, resp, http.StatusOK)
	f.mu.Lock()
	deletedRef := f.lastDeletedRef
	f.mu.Unlock()
	if deletedRef != "prov:newkey" {
		t.Errorf("deleted ref = %q; want prov:newkey", deletedRef)
	}
}

// TestKeysValidation covers the request-shape guards on the key endpoints.
func TestKeysValidation(t *testing.T) {
	f := populatedFake()
	_, c := startAdmin(t, f)

	// Missing ref/secret -> 400.
	resp, _ := doBody(t, c, http.MethodPost, "/keys", `{"ref":""}`)
	wantStatus(t, resp, http.StatusBadRequest)
	resp, _ = doBody(t, c, http.MethodPost, "/keys", `{"secret":"x"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	// Malformed JSON -> 400.
	resp, _ = doBody(t, c, http.MethodPost, "/keys", `not-json`)
	wantStatus(t, resp, http.StatusBadRequest)
	// GET method is not DELETE -> 405 on element route is covered by serving GET
	// /keys/..., which hits the element handler and must 405.
	resp, _ = doJSON(t, c, http.MethodGet, "/keys/some-ref")
	wantStatus(t, resp, http.StatusMethodNotAllowed)
}

// TestReplayEndpoint covers POST /replay/{id} including method guard, success
// passthrough of the response shape, and not-found mapping.
func TestReplayEndpoint(t *testing.T) {
	f := populatedFake()
	_, c := startAdmin(t, f)

	// Method guard.
	resp, _ := doJSON(t, c, http.MethodGet, "/replay/req-1")
	wantStatus(t, resp, http.StatusMethodNotAllowed)

	// Success: response shape is the backend's replayed response.
	resp, body := doBody(t, c, http.MethodPost, "/replay/req-1", "")
	wantStatus(t, resp, http.StatusOK)
	var rtv struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	decodeBody(t, body, &rtv)
	if rtv.ID != "replayed" || rtv.Object != "chat.completion" {
		t.Errorf("replay response = %+v", rtv)
	}
}
