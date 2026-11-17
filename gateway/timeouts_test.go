package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GJH1203/sous/internal/fakeupstream"
)

func TestDeadline(t *testing.T) {
	arrived := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	const total = 10 * time.Second
	tests := []struct {
		header string
		want   time.Duration // after arrived; 0 for an error
	}{
		{"", total},
		{"2500", 2500 * time.Millisecond},
		{"10000", total},
		{"10001", total},
		{"99999999999999999", total}, // would overflow as a Duration
		{"1", time.Millisecond},
		{"0", 0},
		{"-5", 0},
		{"1.5", 0},
		{"2s", 0},
		{"soon", 0},
		{"99999999999999999999", 0}, // not an int64
	}
	for _, tt := range tests {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		if tt.header != "" {
			r.Header.Set("X-Timeout-Ms", tt.header)
		}
		got, err := deadline(r, arrived, total)
		switch {
		case tt.want == 0 && err == nil:
			t.Errorf("%q: deadline %v, want an error", tt.header, got.Sub(arrived))
		case tt.want != 0 && err != nil:
			t.Errorf("%q: %v", tt.header, err)
		case tt.want != 0 && got.Sub(arrived) != tt.want:
			t.Errorf("%q: deadline %v after arrival, want %v", tt.header, got.Sub(arrived), tt.want)
		}
	}
}

// plainChat asks for a reply that is not streamed.
const plainChat = `{"model":"chat","messages":[{"role":"user","content":"hi"}]}`

// stuck is a network where connecting never succeeds: it waits until the
// dial gives up.
func stuck(ctx context.Context, network, addr string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// socketDeadline fails the way a real dial to a host that is down often
// does: the socket's deadline, which the dialer takes from ctx, fires a
// moment before ctx's own timer.
func socketDeadline(ctx context.Context, network, addr string) (net.Conn, error) {
	d, _ := ctx.Deadline()
	time.Sleep(time.Until(d) - time.Millisecond)
	return nil, &net.OpError{Op: "dial", Net: network, Err: os.ErrDeadlineExceeded}
}

// gave504 checks that a request was answered with 504 and the timeout's
// code, after about after, and that the access log names the timeout.
func gave504(t *testing.T, r rig, body string, header http.Header, code string, after time.Duration) {
	t.Helper()
	began := time.Now()
	resp, b := post(t, r.gatewayURL+"/v1/chat/completions", header, body)
	took := time.Since(began)
	if e := gatewayError(t, b); resp.StatusCode != 504 || e.Code != code {
		t.Errorf("got %d %s, want 504 %s", resp.StatusCode, b, code)
	}
	if took < after || took > after+time.Second {
		t.Errorf("answered after %v, want about %v", took, after)
	}
	line := accessLine(t, r.logs, func(l map[string]any) bool { return l["status"] == 504.0 })
	if want := strings.TrimSuffix(code, "_timeout"); line["timeout"] != want {
		t.Errorf("access log timeout = %v, want %s", line["timeout"], want)
	}
}

func TestConnectTimeout(t *testing.T) {
	for name, dial := range map[string]dialFunc{"context": stuck, "socket deadline": socketDeadline} {
		t.Run(name, func(t *testing.T) {
			r := setup(t, options{
				timeouts: Timeouts{Connect: 100 * time.Millisecond, FirstByte: 2 * time.Second, Idle: time.Second, Total: 5 * time.Second},
				dial:     dial,
			})
			gave504(t, r, chatBody, nil, "connect_timeout", 90*time.Millisecond)
			if n := r.fake.Stats().Requests; n != 0 {
				t.Errorf("the upstream got %d requests through a network that is down", n)
			}
		})
	}
}

// The first byte of the body, not the headers: a stream whose headers come
// at once but whose first event does not is just as slow.
func TestFirstByteTimeout(t *testing.T) {
	tests := []struct {
		name string
		body string
		fake func(*fakeupstream.Config)
	}{
		{"headers late", plainChat, func(c *fakeupstream.Config) { c.Delay = time.Minute }},
		{"stream headers on time, first event late", chatBody, func(c *fakeupstream.Config) { c.PromptDelay = time.Minute }},
		{"whole reply late", plainChat, func(c *fakeupstream.Config) { c.PromptDelay = time.Minute }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := setup(t, options{
				timeouts: Timeouts{Connect: time.Second, FirstByte: 200 * time.Millisecond, Idle: time.Second, Total: 5 * time.Second},
				fake:     tt.fake,
			})
			gave504(t, r, tt.body, nil, "first_byte_timeout", 200*time.Millisecond)
			// Giving up also ends the upstream request.
			waitFor(t, time.Second, "the upstream request to end", func() bool { return r.fake.Stats().Concurrent == 0 })
		})
	}
}

// A request that waits in the gateway's queue past its deadline gets 504,
// though the queue timeout is longer. The caller sets the deadline here, so
// the stream holding the slot keeps the model's own, longer one.
func TestTotalTimeoutWhileQueued(t *testing.T) {
	r := setup(t, options{maxConcurrent: 1, fake: slowFake})
	holdSlot(t, r)
	gave504(t, r, chatBody, http.Header{"X-Timeout-Ms": {"200"}}, "total_timeout", 200*time.Millisecond)
	if n := r.fake.Stats().Requests; n != 1 {
		t.Errorf("the upstream got %d requests, want only the one holding the slot", n)
	}
}

// streamEnd sends chatBody and returns the status, the last event, and how
// long the whole stream took.
func streamEnd(t *testing.T, r rig) (int, string, time.Duration) {
	t.Helper()
	began := time.Now()
	resp, body := post(t, r.gatewayURL+"/v1/chat/completions", nil, chatBody)
	events := splitBody(body)
	return resp.StatusCode, events[len(events)-1], time.Since(began)
}

func TestIdleTimeout(t *testing.T) {
	r := setup(t, options{
		timeouts: Timeouts{Connect: time.Second, FirstByte: time.Second, Idle: 200 * time.Millisecond, Total: 5 * time.Second},
		fake:     slowFake, // the first event, then a minute of nothing
	})
	status, last, took := streamEnd(t, r)
	if status != 200 || !strings.HasPrefix(last, "event: error") || !strings.Contains(last, `"code":"idle_timeout"`) {
		t.Errorf("got %d, ending with %q; want 200 and an idle_timeout error event", status, last)
	}
	if took < 200*time.Millisecond || took > time.Second {
		t.Errorf("the stream ended after %v, want about the idle timeout, 200ms", took)
	}
	line := accessLine(t, r.logs, func(l map[string]any) bool { return l["stream"] == true })
	check(t, line, map[string]any{"status": 200.0, "timeout": "idle"})
	waitFor(t, time.Second, "the upstream request to end", func() bool { return r.fake.Stats().Concurrent == 0 })
}

// Events closer together than the idle timeout keep a stream going for
// longer than the idle timeout.
func TestSteadyStreamOutlastsIdleTimeout(t *testing.T) {
	r := setup(t, options{
		timeouts: Timeouts{Connect: time.Second, FirstByte: time.Second, Idle: 150 * time.Millisecond, Total: 5 * time.Second},
		fake: func(c *fakeupstream.Config) {
			c.Chunks = 6
			c.ChunkInterval = 60 * time.Millisecond // 360 ms in all
		},
	})
	if status, last, _ := streamEnd(t, r); status != 200 || last != "data: [DONE]" {
		t.Errorf("got %d, ending with %q; want the whole stream", status, last)
	}
	line := accessLine(t, r.logs, func(l map[string]any) bool { return l["stream"] == true })
	check(t, line, map[string]any{"status": 200.0, "timeout": ""})
}

func TestTotalTimeoutDuringStream(t *testing.T) {
	r := setup(t, options{
		timeouts: Timeouts{Connect: time.Second, FirstByte: 200 * time.Millisecond, Idle: time.Second, Total: 400 * time.Millisecond},
		fake: func(c *fakeupstream.Config) {
			c.Chunks = 100
			c.ChunkInterval = 50 * time.Millisecond // never idle, but 5 s in all
		},
	})
	status, last, took := streamEnd(t, r)
	if status != 200 || !strings.Contains(last, `"code":"total_timeout"`) {
		t.Errorf("got %d, ending with %q; want 200 and a total_timeout error event", status, last)
	}
	if took < 400*time.Millisecond || took > 1400*time.Millisecond {
		t.Errorf("the stream ended after %v, want about the total timeout, 400ms", took)
	}
	line := accessLine(t, r.logs, func(l map[string]any) bool { return l["stream"] == true })
	check(t, line, map[string]any{"timeout": "total"})
}

// The upstream learns how long it has left, which is no more than the
// model's total timeout, nor than the caller's own deadline.
func TestTimeLeftGoesUpstream(t *testing.T) {
	r := setup(t, options{}) // a total of 10 s
	tests := []struct {
		header string
		max    int
	}{
		{"", 10_000},
		{"3000", 3000},
		{"99999999", 10_000},
	}
	for _, tt := range tests {
		var h http.Header
		if tt.header != "" {
			h = http.Header{"X-Timeout-Ms": {tt.header}}
		}
		if resp, _ := post(t, r.gatewayURL+"/v1/embeddings", h, embedBody); resp.StatusCode != 200 {
			t.Fatalf("%q: %d", tt.header, resp.StatusCode)
		}
		left, err := strconv.Atoi(r.fake.Last().Header.Get("X-Timeout-Ms"))
		if err != nil || left > tt.max || left < tt.max-1000 {
			t.Errorf("caller sent %q; upstream got X-Timeout-Ms %q, want a little under %d",
				tt.header, r.fake.Last().Header.Get("X-Timeout-Ms"), tt.max)
		}
	}
}

func TestBadTimeoutHeaderGets400(t *testing.T) {
	r := setup(t, options{})
	for _, v := range []string{"0", "-1", "1.5", "20s", "soon"} {
		resp, body := post(t, r.gatewayURL+"/v1/embeddings", http.Header{"X-Timeout-Ms": {v}}, embedBody)
		if e := gatewayError(t, body); resp.StatusCode != 400 || e.Code != "invalid_timeout" {
			t.Errorf("%q: got %d %s", v, resp.StatusCode, body)
		}
	}
	if n := r.fake.Stats().Requests; n != 0 {
		t.Errorf("%d bad requests reached the upstream", n)
	}
}

// A body that ends before its first byte is an answer, not a failure.
func TestEmptyBodyIsAnAnswer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	gw, client := startGateway(t, upstream.URL, "")
	t.Cleanup(func() {
		gw.Close()
		client.CloseIdleConnections()
	})
	if resp, body := post(t, gw.URL+"/v1/embeddings", nil, embedBody); resp.StatusCode != 204 || len(body) != 0 {
		t.Errorf("got %d %q, want the upstream's empty 204", resp.StatusCode, body)
	}
}
