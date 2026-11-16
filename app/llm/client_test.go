package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/GJH1203/sous/internal/fakeupstream"
)

const testKey = "test-app-key"

// fake stands in for the gateway: it speaks the same protocol.
func fake(t *testing.T, change func(*fakeupstream.Config)) (*fakeupstream.Server, *Client) {
	t.Helper()
	cfg := fakeupstream.DefaultConfig()
	cfg.ChunkInterval = 0
	if change != nil {
		change(&cfg)
	}
	f, err := fakeupstream.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(f)
	t.Cleanup(func() {
		f.Close()
		srv.Close()
	})
	return f, New(srv.URL+"/", testKey) // the trailing slash is dropped
}

func TestChat(t *testing.T) {
	f, c := fake(t, func(cfg *fakeupstream.Config) { cfg.Reply = "番茄炒蛋" })
	req := ChatRequest{Model: Extract, Messages: []Message{
		{Role: "system", Content: "extract"},
		{Role: "user", Content: "今天做了番茄炒蛋"},
	}}
	got, err := c.Chat(context.Background(), "record", req)
	if err != nil || got != "番茄炒蛋" {
		t.Fatalf("Chat: %q, %v", got, err)
	}
	last := f.Last()
	if last.Path != "/v1/chat/completions" {
		t.Errorf("path %s", last.Path)
	}
	for name, want := range map[string]string{
		"Authorization": "Bearer " + testKey,
		"X-Feature":     "record",
		"Content-Type":  "application/json",
		"X-Timeout-Ms":  "180000",
	} {
		if got := last.Header.Get(name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	var sent ChatRequest
	if err := json.Unmarshal(last.Body, &sent); err != nil || sent.Model != Extract || !slices.Equal(sent.Messages, req.Messages) {
		t.Errorf("sent %s", last.Body)
	}
}

func TestEmbed(t *testing.T) {
	f, c := fake(t, func(cfg *fakeupstream.Config) { cfg.Dims = 4 })
	ab, err := c.Embed(context.Background(), "search", []string{"番茄", "茄子"})
	if err != nil || len(ab) != 2 || len(ab[0]) != 4 {
		t.Fatalf("Embed: %v, %v", ab, err)
	}
	if !strings.Contains(string(f.Last().Body), `"model":"embed"`) {
		t.Errorf("sent %s", f.Last().Body)
	}
	ba, err := c.Embed(context.Background(), "search", []string{"茄子", "番茄"})
	if err != nil || !slices.Equal(ab[0], ba[1]) || !slices.Equal(ab[1], ba[0]) || slices.Equal(ab[0], ab[1]) {
		t.Errorf("vectors do not follow their inputs: %v %v", ab, ba)
	}
}

// The list may come in any order; each vector's index says which input it
// belongs to.
func TestEmbedFollowsIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"index":1,"embedding":[1]},{"index":0,"embedding":[0]}]}`)
	}))
	defer srv.Close()
	got, err := New(srv.URL, testKey).Embed(context.Background(), "search", []string{"a", "b"})
	if err != nil || got[0][0] != 0 || got[1][0] != 1 {
		t.Errorf("Embed: %v, %v", got, err)
	}
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name  string
		reply func(w http.ResponseWriter)
		want  Error
	}{
		{"gateway error", func(w http.ResponseWriter) {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":{"message":"the API key is not valid","type":"invalid_request_error","code":"invalid_api_key"}}`)
		}, Error{401, "invalid_request_error", "the API key is not valid"}},
		{"upstream error, numeric code", func(w http.ResponseWriter) {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"code":400,"message":"the request exceeds the available context size","type":"invalid_request_error"}}`)
		}, Error{400, "invalid_request_error", "the request exceeds the available context size"}},
		{"not JSON", func(w http.ResponseWriter) {
			w.WriteHeader(502)
			fmt.Fprint(w, "<html>Bad Gateway</html>")
		}, Error{502, "unknown", "Bad Gateway"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tt.reply(w) }))
			defer srv.Close()
			_, err := New(srv.URL, testKey).Chat(context.Background(), "record", ChatRequest{Model: Extract})
			var e *Error
			if !errors.As(err, &e) || *e != tt.want {
				t.Fatalf("got %#v, want %#v", err, tt.want)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("the key is in the error: %v", err)
			}
		})
	}
}

func TestUpstreamFailure(t *testing.T) {
	_, c := fake(t, func(cfg *fakeupstream.Config) { cfg.FailFirst = 1 })
	req := ChatRequest{Model: Chat, Messages: []Message{{Role: "user", Content: "hi"}}}
	_, err := c.Chat(context.Background(), "record", req)
	var e *Error
	if !errors.As(err, &e) || e.Status != 503 {
		t.Fatalf("got %v, want a 503", err)
	}
	if _, err := c.Chat(context.Background(), "record", req); err != nil {
		t.Errorf("second try: %v", err)
	}
}

func TestReady(t *testing.T) {
	for _, status := range []int{200, 503} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/readyz" || r.Header.Get("Authorization") != "" {
				t.Errorf("%s %s with Authorization %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			}
			w.WriteHeader(status)
		}))
		err := New(srv.URL, testKey).Ready(context.Background())
		if (err == nil) != (status == 200) {
			t.Errorf("status %d: %v", status, err)
		}
		srv.Close()
	}
}
