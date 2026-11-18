package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GJH1203/sous/internal/fakeupstream"
)

// accessLine waits for the access log line that matches, and returns it.
func accessLine(t *testing.T, logs *lockedBuffer, match func(map[string]any) bool) map[string]any {
	t.Helper()
	var found map[string]any
	// The line is written once the handler returns, which can be a moment
	// after the caller has read the whole response.
	waitFor(t, time.Second, "the access log line", func() bool {
		for _, line := range logs.lines(t) {
			if line["msg"] == "request" && match(line) {
				found = line
				return true
			}
		}
		return false
	})
	return found
}

func withID(id string) func(map[string]any) bool {
	return func(line map[string]any) bool { return line["request_id"] == id }
}

// check compares the line's fields with want. JSON numbers decode as
// float64, so want writes them that way.
func check(t *testing.T, line map[string]any, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s = %v, want %v, in %v", k, line[k], v, line)
		}
	}
}

func ms(line map[string]any, k string) float64 {
	v, _ := line[k].(float64)
	return v
}

func TestAccessLogLine(t *testing.T) {
	r := setup(t, options{fake: func(c *fakeupstream.Config) { c.Delay = 50 * time.Millisecond }})
	resp, _ := post(t, r.gatewayURL+"/v1/embeddings", http.Header{"X-Feature": {"search"}}, embedBody)
	line := accessLine(t, r.logs, withID(resp.Header.Get("X-Request-ID")))
	check(t, line, map[string]any{
		"caller": "test", "feature": "search", "path": "/v1/embeddings",
		"model": "embed", "upstream": "fake", "stream": false, "status": 200.0, "attempts": 1.0,
	})
	// The upstream holds its headers back for 50 ms.
	if first, total := ms(line, "first_byte_ms"), ms(line, "total_ms"); first < 50 || total < first {
		t.Errorf("first byte after %v ms, done after %v ms", first, total)
	}

	// Five chunks 30 ms apart: the first byte comes long before the end.
	r.fake.Update(func(c *fakeupstream.Config) {
		c.Delay = 0
		c.ChunkInterval = 30 * time.Millisecond
	})
	resp, _ = post(t, r.gatewayURL+"/v1/chat/completions", nil, chatBody)
	line = accessLine(t, r.logs, withID(resp.Header.Get("X-Request-ID")))
	check(t, line, map[string]any{"model": "chat", "stream": true, "status": 200.0, "feature": ""})
	if first, total := ms(line, "first_byte_ms"), ms(line, "total_ms"); total < 150 || first > total/2 {
		t.Errorf("first byte after %v ms, done after %v ms", first, total)
	}
}

func TestAccessLogOutcomes(t *testing.T) {
	r := setup(t, options{})
	tests := []struct {
		name   string
		auth   string
		body   string
		fake   func(*fakeupstream.Config)
		status float64
		want   map[string]any
	}{
		{"no key", "", embedBody, nil, 401, map[string]any{"caller": "", "model": ""}},
		{"unknown model", "Bearer " + testKey, `{"model":"gpt-4o","input":"x"}`, nil, 404,
			map[string]any{"caller": "test", "model": "gpt-4o", "upstream": ""}},
		{"upstream error", "Bearer " + testKey, embedBody, func(c *fakeupstream.Config) {
			c.FailRate = 1
			c.FailStatus = 503
		}, 503, map[string]any{"model": "embed", "upstream": "fake"}},
	}
	for _, tt := range tests {
		if tt.fake != nil {
			r.fake.Update(tt.fake)
		}
		req, _ := http.NewRequest("POST", r.gatewayURL+"/v1/embeddings", strings.NewReader(tt.body))
		if tt.auth != "" {
			req.Header.Set("Authorization", tt.auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		line := accessLine(t, r.logs, withID(resp.Header.Get("X-Request-ID")))
		tt.want["status"] = tt.status
		check(t, line, tt.want)
	}
}

// A panic is logged as the 500 the caller got, because the access log sits
// outside the recovery.
func TestAccessLogAfterPanic(t *testing.T) {
	url, logs := startHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	resp, _ := send(t, "POST", url+"/v1/embeddings", "Bearer app-key")
	line := accessLine(t, logs, withID(resp.Header.Get("X-Request-ID")))
	check(t, line, map[string]any{"caller": "app", "status": 500.0})
}

func TestAccessLogCallerLeft(t *testing.T) {
	r := setup(t, options{fake: func(c *fakeupstream.Config) {
		c.Chunks = 100
		c.ChunkInterval = time.Minute
	}})
	startStream(t, r.gatewayURL)()
	line := accessLine(t, r.logs, func(map[string]any) bool { return true })
	check(t, line, map[string]any{"status": 499.0, "stream": true, "model": "chat"})
}

// The proxy's own warnings carry the request ID, so they can be put next to
// the access log line.
func TestWarningsCarryRequestID(t *testing.T) {
	r := setup(t, options{fake: func(c *fakeupstream.Config) { c.BreakAfter = 2 }})
	resp, _ := post(t, r.gatewayURL+"/v1/chat/completions", nil, chatBody)
	id := resp.Header.Get("X-Request-ID")
	accessLine(t, r.logs, withID(id))
	for _, line := range r.logs.lines(t) {
		if line["msg"] == "upstream stream broke off" {
			if line["request_id"] != id {
				t.Errorf("warning has request ID %v, want %s", line["request_id"], id)
			}
			return
		}
	}
	t.Errorf("no warning about the broken stream:\n%s", r.logs)
}

func TestLogsHoldNoSecrets(t *testing.T) {
	r := setup(t, options{
		apiKey: "upstream-secret",
		fake:   func(c *fakeupstream.Config) { c.Reply = "reply-secret" },
	})
	body := `{"model":"chat","messages":[{"role":"user","content":"prompt-secret"}]}`
	streamed := `{"model":"chat","stream":true,"messages":[{"role":"user","content":"prompt-secret"}]}`
	post(t, r.gatewayURL+"/v1/chat/completions", nil, body)
	post(t, r.gatewayURL+"/v1/chat/completions", nil, streamed)
	post(t, r.gatewayURL+"/v1/chat/completions", http.Header{"Authorization": {"Bearer wrong-secret"}}, body)
	r.fake.Update(func(c *fakeupstream.Config) { c.BreakAfter = 1 })
	post(t, r.gatewayURL+"/v1/chat/completions", nil, streamed)
	r.fake.Update(func(c *fakeupstream.Config) {
		c.BreakAfter = 0
		c.FailRate = 1
		c.FailStatus = 400
	})
	resp, _ := post(t, r.gatewayURL+"/v1/chat/completions", nil, body)
	accessLine(t, r.logs, withID(resp.Header.Get("X-Request-ID")))

	logs := r.logs.String()
	for _, secret := range []string{testKey, "upstream-secret", "wrong-secret", "prompt-secret", "reply-secret"} {
		if strings.Contains(logs, secret) {
			t.Errorf("%q is in the log:\n%s", secret, logs)
		}
	}
	if n := strings.Count(logs, `"msg":"request"`); n != 5 {
		t.Errorf("%d access log lines for 5 requests:\n%s", n, logs)
	}
}

func TestRecorder(t *testing.T) {
	// The status of the first WriteHeader, which is the one the server sends.
	rec := &recorder{ResponseWriter: httptest.NewRecorder(), start: time.Now()}
	rec.WriteHeader(404)
	rec.WriteHeader(500)
	rec.Write([]byte("x"))
	if rec.status != 404 || rec.firstByte <= 0 {
		t.Errorf("status %d, first byte after %v", rec.status, rec.firstByte)
	}
	// Flushing goes through to the server's writer.
	inner := httptest.NewRecorder()
	rec = &recorder{ResponseWriter: inner, start: time.Now()}
	if err := http.NewResponseController(rec).Flush(); err != nil || !inner.Flushed {
		t.Errorf("flush: %v, flushed %v", err, inner.Flushed)
	}
}
