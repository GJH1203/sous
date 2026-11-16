package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GJH1203/sous/internal/fakeupstream"
)

// twoUpstreams starts fakes "a" and "b" behind one gateway: embed goes to a,
// chat to b. Only b takes a key, and each knows its models by its own names.
func twoUpstreams(t *testing.T) (a, b *fakeupstream.Server, gatewayURL string) {
	t.Helper()
	start := func(name string) (*fakeupstream.Server, string) {
		cfg := fakeupstream.DefaultConfig()
		cfg.Name = name
		cfg.ChunkInterval = 0
		fake, err := fakeupstream.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(fake)
		t.Cleanup(func() {
			fake.Close()
			srv.Close()
		})
		return fake, srv.URL
	}
	a, aURL := start("a")
	b, bURL := start("b")
	gw, client := startGatewayWith(t, &Config{
		Upstreams: []Upstream{
			{Name: "a", BaseURL: aURL, MaxConcurrent: 100, QueueTimeout: 10 * time.Second},
			{Name: "b", BaseURL: bURL, APIKey: "b-key", MaxConcurrent: 100, QueueTimeout: 10 * time.Second},
		},
		Models: map[string]ModelRoute{
			"embed": {Upstream: "a", Model: "a-embedder"},
			"chat":  {Upstream: "b", Model: "b-chat"},
		},
	}, 5*time.Second)
	t.Cleanup(func() {
		gw.Close()
		client.CloseIdleConnections()
	})
	return a, b, gw.URL
}

// sentModel is the model named in the last request an upstream received.
func sentModel(t *testing.T, fake *fakeupstream.Server) string {
	t.Helper()
	var body struct{ Model string }
	if err := json.Unmarshal(fake.Last().Body, &body); err != nil {
		t.Fatal(err)
	}
	return body.Model
}

func TestRoutesByModel(t *testing.T) {
	a, b, gw := twoUpstreams(t)
	tests := []struct {
		path, body string
		fake       *fakeupstream.Server
		name       string // in X-Fake-Upstream
		model      string
		auth       string
	}{
		{"/v1/embeddings", embedBody, a, "a", "a-embedder", ""},
		{"/v1/chat/completions", `{"model":"chat","messages":[{"role":"user","content":"hi"}]}`, b, "b", "b-chat", "Bearer b-key"},
	}
	for _, tt := range tests {
		resp, body := post(t, gw+tt.path, nil, tt.body)
		if resp.StatusCode != 200 || resp.Header.Get("X-Fake-Upstream") != tt.name {
			t.Errorf("%s: %d from %q, want 200 from %q: %s", tt.path, resp.StatusCode, resp.Header.Get("X-Fake-Upstream"), tt.name, body)
			continue
		}
		if m := sentModel(t, tt.fake); m != tt.model {
			t.Errorf("%s: upstream %s got model %q, want %q", tt.path, tt.name, m, tt.model)
		}
		if got := tt.fake.Last().Header.Get("Authorization"); got != tt.auth {
			t.Errorf("%s: upstream %s got Authorization %q, want %q", tt.path, tt.name, got, tt.auth)
		}
	}
	if na, nb := a.Stats().Requests, b.Stats().Requests; na != 1 || nb != 1 {
		t.Errorf("a got %d requests, b got %d; want 1 each", na, nb)
	}
}

func TestUnknownFieldsReachUpstream(t *testing.T) {
	_, b, gw := twoUpstreams(t)
	sent := `{"model":"chat","messages":[{"role":"user","content":"hi"}],` +
		`"x_made_up":{"nested":[1,2.50,null,"<&>"]},"temperature":0.2,"seed":12345678901234567890}`
	if resp, body := post(t, gw+"/v1/chat/completions", nil, sent); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var want, got map[string]any
	json.Unmarshal([]byte(sent), &want)
	if err := json.Unmarshal(b.Last().Body, &got); err != nil {
		t.Fatal(err)
	}
	want["model"] = "b-chat"
	if !jsonEqual(got, want) {
		t.Errorf("upstream got  %s\nwant the same as %s", b.Last().Body, sent)
	}
	// Numbers stay as written: decoding into float64 would turn the seed
	// into 12345678901234567000.
	if !strings.Contains(string(b.Last().Body), `"seed":12345678901234567890`) {
		t.Errorf("seed changed: %s", b.Last().Body)
	}
}

// jsonEqual compares two decoded JSON values.
func jsonEqual(x, y any) bool {
	bx, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return string(bx) == string(by)
}

func TestBadRequestsAreNotForwarded(t *testing.T) {
	a, b, gw := twoUpstreams(t)
	tests := []struct {
		body   string
		status int
		code   string
	}{
		{`not json`, 400, "invalid_json"},
		{`["model","chat"]`, 400, "invalid_json"},
		{`null`, 400, "missing_model"},
		{`{"input":"x"}`, 400, "missing_model"},
		{`{"model":3}`, 400, "missing_model"},
		{`{"model":null}`, 400, "missing_model"},
		{`{"model":""}`, 400, "missing_model"},
		{`{"model":"gpt-4o","input":"x"}`, 404, "model_not_found"},
		{`{"model":"Embed","input":"x"}`, 404, "model_not_found"}, // names are case-sensitive
	}
	for _, tt := range tests {
		resp, body := post(t, gw+"/v1/embeddings", nil, tt.body)
		e := gatewayError(t, body)
		if resp.StatusCode != tt.status || e.Code != tt.code || e.Type != "invalid_request_error" {
			t.Errorf("%s: %d %s; want %d %s", tt.body, resp.StatusCode, body, tt.status, tt.code)
		}
	}
	if na, nb := a.Stats().Requests, b.Stats().Requests; na+nb != 0 {
		t.Errorf("the upstreams got %d requests", na+nb)
	}
}

// With "model" given twice, Go keeps the last; the upstream must not see the
// other one, or it could act on a model the gateway did not route by.
func TestUpstreamSeesOnlyTheRoutedModel(t *testing.T) {
	a, _, gw := twoUpstreams(t)
	post(t, gw+"/v1/embeddings", nil, `{"model":"chat","input":"x","model":"embed"}`)
	if got := string(a.Last().Body); got != `{"input":"x","model":"a-embedder"}` {
		t.Errorf("upstream got %s", got)
	}
}
