package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/effecttrace/effecttrace/internal/correlate"
	"github.com/effecttrace/effecttrace/internal/store"
	"github.com/effecttrace/effecttrace/internal/synth"
)

func server(t *testing.T, token string) (*httptest.Server, string) {
	c := synth.New(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	c.Sources()
	w := c.Deployment("shop", "checkout", 2)
	c.Advance(time.Minute)
	trace, finish := c.MCPCall("restart_workload", synth.Request{Verb: "patch", Res: "deployments", NS: "shop", Name: "checkout"}, w.Deploy, synth.MCPOptions{})
	w.Mutate()
	finish()
	w.Rollout(false)
	st := store.New(store.DefaultConfig())
	for _, r := range c.Records() {
		st.Apply(r)
	}
	now := c.Now.Add(time.Hour)
	s := &Server{Engine: correlate.NewEngine(st, correlate.DefaultConfig(), correlate.Options{}), Store: st, Version: "test", Token: token, Now: func() time.Time { return now }}
	srv := httptest.NewServer(s.Handler(nil))
	t.Cleanup(srv.Close)
	return srv, trace
}

func get(t *testing.T, url, token string) (int, string, http.Header) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, b.String(), resp.Header
}

func TestRoutes(t *testing.T) {
	srv, trace := server(t, "")
	code, body, h := get(t, srv.URL+"/api/v1/traces/"+trace+"/actions", "")
	if code != 200 || !strings.Contains(body, "mcp-") || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("trace lookup: %d %s", code, body)
	}
	id := body[strings.Index(body, "mcp-"):]
	id = id[:strings.IndexByte(id, '"')]
	if code, body, _ := get(t, srv.URL+"/api/v1/effects/"+id, ""); code != 200 || !strings.Contains(body, `"schemaVersion": "effecttrace.io/v1alpha1"`) {
		t.Fatalf("effects: %d %s", code, body[:min(200, len(body))])
	}
	for _, f := range []string{"text", "dot", "mermaid"} {
		if code, _, _ := get(t, srv.URL+"/api/v1/effects/"+id+"/graph?format="+f, ""); code != 200 {
			t.Errorf("format %s: %d", f, code)
		}
	}
	for path, want := range map[string]int{
		"/api/v1/effects/nope":                        404,
		"/api/v1/effects/bad%20id%3Cscript%3E":        400,
		"/api/v1/traces/xyz/actions":                  400,
		"/api/v1/actions?since=yesterday":             400,
		"/api/v1/actions?limit=0":                     400,
		"/api/v1/effects/" + id + "/graph?format=svg": 400,
		"/healthz":       200,
		"/api/v1/status": 200,
	} {
		if code, _, _ := get(t, srv.URL+path, ""); code != want {
			t.Errorf("%s: %d, want %d", path, code, want)
		}
	}
}

func TestBearerToken(t *testing.T) {
	srv, _ := server(t, "s3cret")
	if code, _, _ := get(t, srv.URL+"/api/v1/actions", ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _, _ := get(t, srv.URL+"/api/v1/actions", "wrong"); code != 401 {
		t.Errorf("wrong token: %d", code)
	}
	if code, _, _ := get(t, srv.URL+"/api/v1/actions", "s3cret"); code != 200 {
		t.Errorf("valid token: %d", code)
	}
	if code, _, _ := get(t, srv.URL+"/healthz", ""); code != 200 {
		t.Errorf("health endpoints must not require the token: %d", code)
	}
}
