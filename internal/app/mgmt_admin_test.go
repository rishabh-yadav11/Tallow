package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

func adminJSON(t *testing.T, method, sock, path string, body []byte) (int, []byte) {
	t.Helper()
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
			DisableKeepAlives: true,
		},
		Timeout: 10 * time.Second,
	}
	req, err := http.NewRequest(method, "http://admin"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build admin request %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("admin %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func configInt(t *testing.T, sock, path string) float64 {
	t.Helper()
	code, body := adminGet(t, sock, path)
	if code != 200 {
		t.Fatalf("GET %s = %d: %s", path, code, body)
	}
	var v float64
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("GET %s is not a number: %v (%s)", path, err, body)
	}
	return v
}

func configString(t *testing.T, sock, path string) string {
	t.Helper()
	code, body := adminGet(t, sock, path)
	if code != 200 {
		t.Fatalf("GET %s = %d: %s", path, code, body)
	}
	var v string
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("GET %s is not a string: %v (%s)", path, err, body)
	}
	return v
}

func TestManagementConfigGetPutRoundTrip(t *testing.T) {
	upSrv, _ := setup(t)
	g := startLive(t, upSrv.URL)

	code, doc := adminGet(t, g.adminSock, "/config")
	if code != http.StatusOK {
		t.Fatalf("GET /config = %d: %s", code, doc)
	}
	var live map[string]any
	if err := json.Unmarshal(doc, &live); err != nil {
		t.Fatalf("GET /config is not JSON: %v (%s)", err, doc)
	}
	if v, _ := live["version"].(float64); v != 1 {
		t.Fatalf("GET /config version = %v; want 1", live["version"])
	}
	if _, ok := live["server"]; !ok {
		t.Fatalf("GET /config missing server section: %s", doc)
	}

	code, res := adminJSON(t, http.MethodPut, g.adminSock, "/config", doc)
	if code != http.StatusOK {
		t.Fatalf("PUT /config with the GET doc = %d: %s", code, res)
	}
	code, doc2 := adminGet(t, g.adminSock, "/config")
	if code != http.StatusOK {
		t.Fatalf("GET /config after PUT = %d: %s", code, doc2)
	}
	var live2 map[string]any
	if err := json.Unmarshal(doc2, &live2); err != nil {
		t.Fatalf("GET /config after PUT is not JSON: %v", err)
	}
	if v, _ := live2["version"].(float64); v != 1 {
		t.Fatalf("GET /config after PUT version = %v; want 1", live2["version"])
	}

	code, res = adminJSON(t, http.MethodPut, g.adminSock, "/config", []byte(`{"version":99,"server":{}}`))
	if code == http.StatusOK {
		t.Fatalf("PUT /config accepted an unsupported version: %s", res)
	}
	code, doc3 := adminGet(t, g.adminSock, "/config")
	if code != http.StatusOK {
		t.Fatalf("GET /config after rejected PUT = %d: %s", code, doc3)
	}
	var live3 map[string]any
	if err := json.Unmarshal(doc3, &live3); err != nil {
		t.Fatalf("doc after rejected PUT unparseable: %v", err)
	}
	if v, _ := live3["version"].(float64); v != 1 {
		t.Errorf("after a rejected PUT the live doc version = %v; want 1: the invalid "+
			"document must not half-swap the registry", live3["version"])
	}
	if code, _ := g.chat(t, "after-rejected-put"); code != http.StatusOK {
		t.Errorf("after a rejected PUT the gateway stopped serving")
	}

	code, _ = adminJSON(t, http.MethodPut, g.adminSock, "/config", []byte(`not json`))
	if code != http.StatusBadRequest {
		t.Errorf("PUT /config with malformed JSON = %d; want 400", code)
	}
}

func TestManagementConfigSectionFieldVerbs(t *testing.T) {
	upSrv, _ := setup(t)
	g := startLive(t, upSrv.URL)

	liveListen := configString(t, g.adminSock, "/config/server/listen")
	adminSockFromConfig := configString(t, g.adminSock, "/config/server/admin_socket")

	code, res := adminJSON(t, http.MethodPut, g.adminSock, "/config/server",
		[]byte(`{"listen":"127.0.0.1:1","max_concurrent":4,"queue_timeout":"5s","read_timeout":"30s","write_timeout":"0s","idle_timeout":"120s","max_request_bytes":8388608,"sticky_ttl":"2m","admin_socket":"`+adminSockFromConfig+`"}`))
	if code != http.StatusOK {
		t.Fatalf("PUT /config/server = %d: %s", code, res)
	}
	if got := configInt(t, g.adminSock, "/config/server/max_concurrent"); got != 4 {
		t.Errorf("after PUT /config/server, max_concurrent = %v; want 4", got)
	}

	liveListen = configString(t, g.adminSock, "/config/server/listen")
	if liveListen != "127.0.0.1:1" {
		t.Fatalf("after PUT /config/server, listen = %q; want the PUT value", liveListen)
	}
	adminSockFromConfig = configString(t, g.adminSock, "/config/server/admin_socket")

	code, res = adminJSON(t, http.MethodPatch, g.adminSock, "/config/server/max_concurrent", []byte(`5`))
	if code != http.StatusOK {
		t.Fatalf("PATCH /config/server/max_concurrent = %d: %s", code, res)
	}
	if got := configInt(t, g.adminSock, "/config/server/max_concurrent"); got != 5 {
		t.Errorf("PATCH field did not stick: max_concurrent = %v; want 5", got)
	}

	code, res = adminJSON(t, http.MethodPatch, g.adminSock, "/config/server", []byte(`{"queue_timeout":"9s"}`))
	if code != http.StatusOK {
		t.Fatalf("PATCH /config/server = %d: %s", code, res)
	}
	if got := configString(t, g.adminSock, "/config/server/queue_timeout"); got != "9s" {
		t.Errorf("after section PATCH, queue_timeout = %q; want 9s", got)
	}
	if got := configString(t, g.adminSock, "/config/server/listen"); got != liveListen {
		t.Errorf("after section PATCH, listen changed to %q; want %q: a shallow "+
			"merge must not touch sibling fields", got, liveListen)
	}
	if got := configString(t, g.adminSock, "/config/server/admin_socket"); got != adminSockFromConfig {
		t.Errorf("after section PATCH, admin_socket changed to %q; want %q",
			got, adminSockFromConfig)
	}

	code, res = adminJSON(t, http.MethodDelete, g.adminSock, "/config/server/max_concurrent", nil)
	if code != http.StatusOK {
		t.Fatalf("DELETE /config/server/max_concurrent = %d: %s", code, res)
	}
	if got := configInt(t, g.adminSock, "/config/server/max_concurrent"); got != 16 {
		t.Errorf("after DELETE, max_concurrent = %v; want the applied default 16", got)
	}
}

func TestManagementKeyFiles(t *testing.T) {
	upSrv, _ := setup(t)
	g := startLive(t, upSrv.URL)

	const plaintext = "sk-mgmt-topsecret-do-not-leak"

	code, res := adminJSON(t, http.MethodPost, g.adminSock, "/config/keys",
		[]byte(`{"ref":"p1:k-mgmt","value":"`+plaintext+`"}`))
	if code != http.StatusOK {
		t.Fatalf("POST /config/keys = %d: %s", code, res)
	}

	code, body := adminGet(t, g.adminSock, "/config/keys")
	if code != http.StatusOK {
		t.Fatalf("GET /config/keys = %d: %s", code, body)
	}
	var refs []string
	if err := json.Unmarshal(body, &refs); err != nil {
		t.Fatalf("GET /config/keys is not a string list: %v (%s)", err, body)
	}
	found := false
	for _, r := range refs {
		if r == "p1:k-mgmt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("GET /config/keys lacks the new ref: %s", body)
	}
	if strings.Contains(string(body), plaintext) {
		t.Fatal("GET /config/keys leaked the plaintext secret")
	}

	code, _ = adminJSON(t, http.MethodPost, g.adminSock, "/config/keys",
		[]byte(`{"ref":"p1:k-mgmt","value":"sk-other"}`))
	if code != http.StatusConflict {
		t.Errorf("POST duplicate ref = %d; want 409", code)
	}

	code, res = adminJSON(t, http.MethodDelete, g.adminSock, "/config/keys/p1:k-mgmt", nil)
	if code != http.StatusOK {
		t.Fatalf("DELETE /config/keys/p1:k-mgmt = %d: %s", code, res)
	}
	code, body = adminGet(t, g.adminSock, "/config/keys")
	if code != http.StatusOK {
		t.Fatalf("GET /config/keys after delete = %d: %s", code, body)
	}
	if err := json.Unmarshal(body, &refs); err != nil {
		t.Fatalf("GET /config/keys after delete unparseable: %v", err)
	}
	for _, r := range refs {
		if r == "p1:k-mgmt" {
			t.Errorf("ref %q still listed after DELETE", r)
		}
	}
}

func TestManagementReplayEndToEnd(t *testing.T) {
	upSrv, _ := setup(t)
	g := startLive(t, upSrv.URL)

	code, body := g.chat(t, "replay-original-request")
	if code != http.StatusOK {
		t.Fatalf("chat status %d: %s", code, body)
	}

	code, logsBody := adminGet(t, g.adminSock, "/logs?limit=10")
	if code != http.StatusOK {
		t.Fatalf("GET /logs = %d: %s", code, logsBody)
	}
	var logs []model.RequestMeta
	if err := json.Unmarshal(logsBody, &logs); err != nil {
		t.Fatalf("GET /logs is not a request list: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("no requests recorded")
	}
	id := logs[0].ID
	if id == "" {
		t.Fatal("latest request has no id")
	}

	var replay []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, r := adminJSON(t, http.MethodPost, g.adminSock, "/replay/"+id, nil)
		if code == http.StatusOK {
			replay = r
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("POST /replay/%s never succeeded (last %d: %s)", id, code, r)
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !strings.Contains(string(replay), "pong") {
		t.Errorf("replay response does not carry the upstream payload: %s", replay)
	}

	seedRequestRows(t, g.cfgPath, 1)
	code, res := adminJSON(t, http.MethodPost, g.adminSock, "/replay/seed-0", nil)
	if code != http.StatusConflict {
		t.Errorf("replay without raw bodies = %d, want 409: %s", code, res)
	}
	if !strings.Contains(string(res), "not captured") {
		t.Errorf("409 body does not explain the missing raw bodies: %s", res)
	}

	code, res = adminJSON(t, http.MethodPost, g.adminSock, "/replay/no-such-id", nil)
	if code != http.StatusNotFound {
		t.Errorf("replay of an unknown id = %d, want 404: %s", code, res)
	}
}
