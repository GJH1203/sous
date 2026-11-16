package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lockedBuffer collects what the gateway logs from its own goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lines decodes the JSON log, one map per line.
func (b *lockedBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(b.String()))
	sc.Buffer(nil, 1<<20) // a stack trace makes a long line
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("not a JSON log line: %s", sc.Text())
		}
		out = append(out, m)
	}
	return out
}

// generous is a rate limit that tests never reach.
var generous = RateLimit{Burst: 1_000_000, PerSecond: 1_000_000}

// startHandler serves NewHandler with h in the proxy's place and two callers,
// and returns its URL and its JSON log.
func startHandler(t *testing.T, h http.Handler) (string, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{}
	cfg := &Config{Callers: []Caller{
		{Name: "app", APIKey: "app-key", RateLimit: generous},
		{Name: "other", APIKey: "other-key", RateLimit: generous},
	}}
	srv := httptest.NewServer(NewHandler(cfg, h, slog.New(slog.NewJSONHandler(logs, nil))))
	t.Cleanup(srv.Close)
	return srv.URL, logs
}

// send makes a request with auth as its Authorization header, or none if
// auth is empty.
func send(t *testing.T, method, url, auth string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(embedBody))
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// echo stands in for the proxy and answers with the caller and the request
// ID that the middleware handed it.
func echo(calls *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprintf(w, "%s %s", callerFrom(r.Context()), requestIDFrom(r.Context()))
	})
}

func TestAuth(t *testing.T) {
	var calls atomic.Int32
	url, _ := startHandler(t, echo(&calls))
	tests := []struct {
		auth   string
		status int
		code   string
	}{
		{"", 401, "missing_api_key"},
		{"Bearer ", 401, "missing_api_key"},
		{"app-key", 401, "missing_api_key"}, // no scheme
		{"Basic app-key", 401, "missing_api_key"},
		{"Bearer wrong", 401, "invalid_api_key"},
		{"Bearer app", 401, "invalid_api_key"}, // the start of a key
		{"Bearer app-keys", 401, "invalid_api_key"},
		{"Bearer app-key", 200, ""},
		{"Bearer other-key", 200, ""},
	}
	for _, tt := range tests {
		resp, body := send(t, "POST", url+"/v1/embeddings", tt.auth)
		if resp.StatusCode != tt.status {
			t.Errorf("Authorization %q: %d %s, want %d", tt.auth, resp.StatusCode, body, tt.status)
			continue
		}
		// The ID is given before the key is checked, so a 401 has one too.
		if resp.Header.Get("X-Request-ID") == "" {
			t.Errorf("Authorization %q: no X-Request-ID", tt.auth)
		}
		if tt.status != 401 {
			continue
		}
		e := gatewayError(t, body)
		if e.Code != tt.code || e.Type != "invalid_request_error" || resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("Authorization %q: %s, WWW-Authenticate %q; want code %s", tt.auth, body, resp.Header.Get("WWW-Authenticate"), tt.code)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("the proxy got %d requests, want only the 2 with a known key", n)
	}
}

func TestCallerAndRequestIDReachProxy(t *testing.T) {
	var calls atomic.Int32
	url, _ := startHandler(t, echo(&calls))
	ids := make(map[string]bool)
	for _, c := range []struct{ key, name string }{{"app-key", "app"}, {"other-key", "other"}, {"app-key", "app"}} {
		resp, body := send(t, "POST", url+"/v1/chat/completions", "Bearer "+c.key)
		caller, id, _ := strings.Cut(string(body), " ")
		if caller != c.name {
			t.Errorf("key %s: the proxy was told caller %q, want %q", c.key, caller, c.name)
		}
		if len(id) != 26 || id != resp.Header.Get("X-Request-ID") {
			t.Errorf("the proxy got request ID %q, the caller %q", id, resp.Header.Get("X-Request-ID"))
		}
		if ids[id] {
			t.Errorf("request ID %s given twice", id)
		}
		ids[id] = true
	}
}

func TestRequestIDGoesUpstreamAndBack(t *testing.T) {
	r := setup(t, options{})
	resp, _ := post(t, r.gatewayURL+"/v1/embeddings", nil, embedBody)
	id := resp.Header.Get("X-Request-ID")
	if got := r.fake.Last().Header.Get("X-Request-ID"); id == "" || got != id {
		t.Errorf("the caller got request ID %q, the upstream %q", id, got)
	}

	// An upstream with an ID of its own does not get to replace ours.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", "upstream-id")
		io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	gw, client := startGateway(t, upstream.URL, "", 5*time.Second)
	defer gw.Close()
	defer client.CloseIdleConnections()
	resp, _ = post(t, gw.URL+"/v1/embeddings", nil, embedBody)
	if ids := resp.Header.Values("X-Request-ID"); len(ids) != 1 || len(ids[0]) != 26 {
		t.Errorf("X-Request-ID %q, want only the gateway's", ids)
	}
}

func TestPanicBecomes500(t *testing.T) {
	url, logs := startHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	// Twice: the gateway is still there for the second request.
	for range 2 {
		resp, body := send(t, "POST", url+"/v1/embeddings", "Bearer app-key")
		if e := gatewayError(t, body); resp.StatusCode != 500 || e.Code != "internal_error" || e.Type != "server_error" {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
		id := resp.Header.Get("X-Request-ID")
		found := false
		for _, line := range logs.lines(t) {
			if line["msg"] == "panic" && line["request_id"] == id {
				found = line["panic"] == "boom" && strings.Contains(fmt.Sprint(line["stack"]), "middleware_test.go")
			}
		}
		if !found {
			t.Errorf("no log line with the panic, request ID %s and stack:\n%s", id, logs)
		}
	}
}

// http.ErrAbortHandler is how a handler asks the server to cut the
// connection; recovery must not turn it into an answer.
func TestAbortStillCutsTheConnection(t *testing.T) {
	url, logs := startHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	req, _ := http.NewRequest("POST", url+"/v1/embeddings", strings.NewReader(embedBody))
	req.Header.Set("Authorization", "Bearer app-key")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatalf("got %d, want the connection cut", resp.StatusCode)
	}
	if strings.Contains(logs.String(), "panic") {
		t.Errorf("an abort was logged as a panic:\n%s", logs)
	}
}

func TestUnknownPathsAndMethods(t *testing.T) {
	url, _ := startHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("%s %s reached the proxy", r.Method, r.URL.Path)
	}))
	tests := []struct {
		method, path, auth string
		status             int
		code, allow        string
	}{
		{"GET", "/v1/embeddings", "Bearer app-key", 405, "method_not_allowed", "POST"},
		{"PUT", "/v1/chat/completions", "Bearer app-key", 405, "method_not_allowed", "POST"},
		{"POST", "/v1/models", "Bearer app-key", 404, "not_found", ""},
		{"POST", "/v1/embeddings/", "Bearer app-key", 404, "not_found", ""},
		{"GET", "/", "Bearer app-key", 404, "not_found", ""},
		// The key is checked first, so a stranger learns nothing about paths.
		{"GET", "/v1/models", "", 401, "missing_api_key", ""},
	}
	for _, tt := range tests {
		resp, body := send(t, tt.method, url+tt.path, tt.auth)
		e := gatewayError(t, body)
		if resp.StatusCode != tt.status || e.Code != tt.code || resp.Header.Get("Allow") != tt.allow {
			t.Errorf("%s %s: %d %s, Allow %q; want %d %s, Allow %q",
				tt.method, tt.path, resp.StatusCode, body, resp.Header.Get("Allow"), tt.status, tt.code, tt.allow)
		}
		if resp.Header.Get("X-Request-ID") == "" {
			t.Errorf("%s %s: no X-Request-ID", tt.method, tt.path)
		}
	}
}
