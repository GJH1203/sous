package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GJH1203/sous/internal/fakeupstream"
)

// rig is a fake upstream with a gateway in front of it.
type rig struct {
	fake       *fakeupstream.Server
	fakeURL    string
	gatewayURL string
}

type options struct {
	apiKey        string
	headerTimeout time.Duration
	fake          func(*fakeupstream.Config)
	// connState, if set, watches the upstream's connections.
	connState func(net.Conn, http.ConnState)
}

func setup(t *testing.T, o options) rig {
	t.Helper()
	cfg := fakeupstream.DefaultConfig()
	cfg.ChunkInterval = 0
	if o.fake != nil {
		o.fake(&cfg)
	}
	fake, err := fakeupstream.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewUnstartedServer(fake)
	upstream.Config.ConnState = o.connState
	upstream.Start()
	if o.headerTimeout == 0 {
		o.headerTimeout = 5 * time.Second
	}
	gw, client := startGateway(t, upstream.URL, o.apiKey, o.headerTimeout)
	t.Cleanup(func() {
		fake.Close() // first: both servers' Close wait for requests it holds
		gw.Close()
		upstream.Close()
		client.CloseIdleConnections()
	})
	return rig{fake: fake, fakeURL: upstream.URL, gatewayURL: gw.URL}
}

// startGateway starts a gateway with one upstream. The upstream knows each
// model by its logical name, so a reply is the same through the gateway as
// direct.
func startGateway(t *testing.T, upstream, apiKey string, headerTimeout time.Duration) (*httptest.Server, *http.Client) {
	t.Helper()
	return startGatewayWith(t, &Config{
		Upstreams: []Upstream{{Name: "fake", BaseURL: upstream, APIKey: apiKey}},
		Models: map[string]ModelRoute{
			"chat":  {Upstream: "fake", Model: "chat"},
			"embed": {Upstream: "fake", Model: "embed"},
		},
	}, headerTimeout)
}

func startGatewayWith(t *testing.T, cfg *Config, headerTimeout time.Duration) (*httptest.Server, *http.Client) {
	t.Helper()
	client := NewClient(headerTimeout)
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))
	return httptest.NewServer(NewProxy(client, cfg, logger)), client
}

func post(t *testing.T, url string, header http.Header, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, values := range header {
		req.Header[name] = values
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

// gatewayError decodes one of the gateway's own errors.
func gatewayError(t *testing.T, body []byte) apiError {
	t.Helper()
	var e struct{ Error apiError }
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("not a gateway error: %s", body)
	}
	return e.Error
}

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", within, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The keys are in the order the gateway writes them back in, so the upstream
// gets these exact bytes.
const embedBody = `{"input":["番茄炒蛋","红烧茄子"],"model":"embed"}`

func TestSameAsDirect(t *testing.T) {
	r := setup(t, options{})
	direct, directBody := post(t, r.fakeURL+"/v1/embeddings", nil, embedBody)
	via, viaBody := post(t, r.gatewayURL+"/v1/embeddings", nil, embedBody)
	if via.StatusCode != direct.StatusCode || string(viaBody) != string(directBody) {
		t.Errorf("via the gateway: %d %s\ndirect: %d %s", via.StatusCode, viaBody, direct.StatusCode, directBody)
	}
	for _, name := range []string{"Content-Type", "X-Fake-Upstream"} {
		if via.Header.Get(name) != direct.Header.Get(name) {
			t.Errorf("%s: %q via the gateway, %q direct", name, via.Header.Get(name), direct.Header.Get(name))
		}
	}
	if got := string(r.fake.Last().Body); got != embedBody {
		t.Errorf("upstream received %s", got)
	}
}

func TestCallerKeyIsReplaced(t *testing.T) {
	tests := []struct {
		apiKey, wantAuth string
	}{
		{"upstream-key", "Bearer upstream-key"},
		{"", ""}, // a local upstream needs no key, and gets none
	}
	for _, tt := range tests {
		r := setup(t, options{apiKey: tt.apiKey})
		post(t, r.gatewayURL+"/v1/embeddings", http.Header{
			"Authorization": {"Bearer caller-key"},
			"Cookie":        {"session=caller-key"},
			"X-Anything":    {"caller-key"},
		}, embedBody)

		got := r.fake.Last().Header
		if auth := got.Get("Authorization"); auth != tt.wantAuth {
			t.Errorf("apiKey %q: upstream got Authorization %q, want %q", tt.apiKey, auth, tt.wantAuth)
		}
		for name, values := range got {
			for _, v := range values {
				if strings.Contains(v, "caller-key") {
					t.Errorf("caller's key reached the upstream in %s: %q", name, v)
				}
			}
		}
		if ct := got.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type %q was not forwarded", ct)
		}
	}
}

func TestUpstreamDown(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close() // nothing listens on its port any more
	gw, client := startGateway(t, down.URL, "", 5*time.Second)
	defer gw.Close()
	defer client.CloseIdleConnections()

	began := time.Now()
	resp, body := post(t, gw.URL+"/v1/embeddings", nil, embedBody)
	if took := time.Since(began); took > time.Second {
		t.Errorf("took %v; a refused connection should fail at once", took)
	}
	e := gatewayError(t, body)
	if resp.StatusCode != 502 || e.Code != "upstream_error" || e.Type != "server_error" {
		t.Errorf("got %d %s", resp.StatusCode, body)
	}
	// The dial error names the upstream; that belongs in the log only.
	if strings.Contains(string(body), strings.TrimPrefix(down.URL, "http://")) {
		t.Errorf("error leaks the upstream address: %s", body)
	}
}

func TestHeaderTimeout(t *testing.T) {
	r := setup(t, options{
		headerTimeout: 100 * time.Millisecond,
		fake:          func(c *fakeupstream.Config) { c.Hang = true },
	})
	began := time.Now()
	resp, body := post(t, r.gatewayURL+"/v1/embeddings", nil, embedBody)
	if took := time.Since(began); took > time.Second {
		t.Errorf("took %v with a 100ms header timeout", took)
	}
	if e := gatewayError(t, body); resp.StatusCode != 504 || e.Code != "upstream_timeout" {
		t.Errorf("got %d %s", resp.StatusCode, body)
	}
	// Giving up also closes the upstream request.
	waitFor(t, time.Second, "the upstream request to end", func() bool { return r.fake.Stats().Concurrent == 0 })
}

func TestUpstreamErrorPassesThrough(t *testing.T) {
	r := setup(t, options{fake: func(c *fakeupstream.Config) {
		c.FailRate = 1
		c.FailStatus = 429
		c.RetryAfter = 2
	}})
	direct, directBody := post(t, r.fakeURL+"/v1/embeddings", nil, embedBody)
	via, viaBody := post(t, r.gatewayURL+"/v1/embeddings", nil, embedBody)
	if via.StatusCode != 429 || string(viaBody) != string(directBody) {
		t.Errorf("via the gateway: %d %s\ndirect: %d %s", via.StatusCode, viaBody, direct.StatusCode, directBody)
	}
	if ra := via.Header.Get("Retry-After"); ra != "2" {
		t.Errorf("Retry-After = %q, want 2", ra)
	}
}

func TestBodyLimit(t *testing.T) {
	r := setup(t, options{})
	// {"input":"xxx...x","model":"embed"} exactly at the limit, and one byte
	// over it.
	at := `{"input":"` + strings.Repeat("x", maxBodyBytes-len(`{"input":"","model":"embed"}`)) + `","model":"embed"}`
	if resp, body := post(t, r.gatewayURL+"/v1/embeddings", nil, at); resp.StatusCode != 200 {
		t.Errorf("a body of exactly %d bytes: %d %.200s", len(at), resp.StatusCode, body)
	}
	r.fake.Reset()
	over := at + " "
	resp, body := post(t, r.gatewayURL+"/v1/embeddings", nil, over)
	if e := gatewayError(t, body); resp.StatusCode != 413 || e.Code != "body_too_large" || e.Type != "invalid_request_error" {
		t.Errorf("a body of %d bytes: %d %s", len(over), resp.StatusCode, body)
	}
	if n := r.fake.Stats().Requests; n != 0 {
		t.Errorf("an oversized body still went upstream (%d requests)", n)
	}
}

func TestHopByHopHeadersDropped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Keep-Alive", "timeout=5, max=100") // llama-server sends this
		// Not "keep-alive, X-Hop": that would drop Keep-Alive through the
		// Connection list, and leave the fixed list untested.
		h.Set("Connection", "X-Hop")
		h.Set("X-Hop", "1")
		h.Set("Proxy-Authenticate", "Basic")
		h.Set("X-Kept", "yes")
		io.WriteString(w, "{}")
	}))
	defer upstream.Close()
	gw, client := startGateway(t, upstream.URL, "", 5*time.Second)
	defer gw.Close()
	defer client.CloseIdleConnections()

	resp, _ := post(t, gw.URL+"/v1/embeddings", nil, embedBody)
	for _, name := range []string{"Keep-Alive", "Connection", "X-Hop", "Proxy-Authenticate"} {
		if v := resp.Header.Get(name); v != "" {
			t.Errorf("%s: %q reached the caller", name, v)
		}
	}
	if resp.Header.Get("X-Kept") != "yes" {
		t.Error("an end-to-end header was dropped")
	}
}

func TestCallerLeavingCancelsUpstream(t *testing.T) {
	r := setup(t, options{fake: func(c *fakeupstream.Config) { c.Delay = 5 * time.Second }})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", r.gatewayURL+"/v1/embeddings", strings.NewReader(embedBody))
	if _, err := http.DefaultClient.Do(req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
	// Far sooner than the 5 s the upstream would otherwise have spent.
	waitFor(t, time.Second, "the upstream request to be cancelled", func() bool { return r.fake.Stats().Concurrent == 0 })
}

func TestConnectionsAreReused(t *testing.T) {
	var conns atomic.Int32
	r := setup(t, options{
		fake: func(c *fakeupstream.Config) { c.Delay = 30 * time.Millisecond },
		connState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateNew {
				conns.Add(1)
			}
		},
	})
	// Three rounds of 8 at once need 8 connections if they are kept between
	// rounds, and about 20 if all but 2 are closed after each round.
	for range 3 {
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				resp, err := http.Post(r.gatewayURL+"/v1/embeddings", "application/json", strings.NewReader(embedBody))
				if err != nil {
					t.Error(err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			})
		}
		wg.Wait()
	}
	if n := conns.Load(); n > 8 {
		t.Errorf("24 requests, at most 8 at a time, opened %d upstream connections", n)
	}
}
