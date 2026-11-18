package gateway

import (
	"context"
	"io"
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

// noJitter makes every backoff 0.
func noJitter() float64 { return 0 }

// half makes every backoff half its ceiling.
func half() float64 { return 0.5 }

func TestRetryable(t *testing.T) {
	for status, want := range map[int]bool{
		200: false, 204: false, 400: false, 401: false, 404: false, 413: false, 500: false, 501: false,
		429: true, 502: true, 503: true, 504: true,
	} {
		if got := retryable(status); got != want {
			t.Errorf("retryable(%d) = %v", status, got)
		}
	}
}

func TestBackoff(t *testing.T) {
	tests := []struct {
		n      int
		random float64
		want   time.Duration
	}{
		{1, 0, 0},
		{1, 0.5, 100 * time.Millisecond},
		{2, 0.5, 200 * time.Millisecond},
		{3, 0.5, 400 * time.Millisecond},
		{4, 0.5, 800 * time.Millisecond},
		{5, 0.5, time.Second}, // the ceiling is 2 s from here on
		{6, 0.5, time.Second},
		{100, 0.5, time.Second}, // a shift would have overflowed
		{1, 0.999, 199800 * time.Microsecond},
	}
	for _, tt := range tests {
		if got := backoff(tt.n, tt.random); got != tt.want {
			t.Errorf("backoff(%d, %v) = %v, want %v", tt.n, tt.random, got, tt.want)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		value string
		want  time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"3", 3 * time.Second},
		{"-1", 0},
		{"1.5", 0},
		{"soon", 0},
		{"99999999999", 24 * time.Hour}, // capped
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
	}
	for _, tt := range tests {
		h := http.Header{}
		if tt.value != "" {
			h.Set("Retry-After", tt.value)
		}
		if got := retryAfter(h, now); got != tt.want {
			t.Errorf("Retry-After %q: %v, want %v", tt.value, got, tt.want)
		}
	}
}

// spent calls spend n times at now, and counts the trues.
func spent(b *retryBudget, now time.Time, n int) int {
	got := 0
	for range n {
		if b.spend(now) {
			got++
		}
	}
	return got
}

func TestRetryBudget(t *testing.T) {
	t.Run("3 with no traffic", func(t *testing.T) {
		var b retryBudget
		if got := spent(&b, noon, 10); got != 3 {
			t.Errorf("%d retries, want 3", got)
		}
	})
	t.Run("a fifth of the first attempts, plus 3", func(t *testing.T) {
		var b retryBudget
		for range 50 {
			b.first(noon)
		}
		if got := spent(&b, noon, 100); got != 13 {
			t.Errorf("%d retries after 50 first attempts, want 13", got)
		}
	})
	t.Run("the window covers 10 s", func(t *testing.T) {
		var b retryBudget
		for i := range 10 {
			at := noon.Add(time.Duration(i) * time.Second)
			for range 5 {
				b.first(at)
			}
		}
		// 50 first attempts over the last 10 s, each second's 5 counting.
		if got := spent(&b, noon.Add(9*time.Second), 100); got != 13 {
			t.Errorf("%d retries, want 13", got)
		}
	})
	t.Run("old seconds drop out", func(t *testing.T) {
		var b retryBudget
		for range 50 {
			b.first(noon)
		}
		spent(&b, noon, 100)
		// 10 s on, the 50 first attempts and the 13 retries are both gone.
		if got := spent(&b, noon.Add(10*time.Second), 100); got != 3 {
			t.Errorf("%d retries 10 s later, want 3", got)
		}
	})
	t.Run("a late clock counts in the newer second", func(t *testing.T) {
		var b retryBudget
		for range 50 {
			b.first(noon.Add(10 * time.Second))
		}
		// noon is in the same slot as noon+10s; it must not clear it.
		b.first(noon)
		if got := spent(&b, noon.Add(10*time.Second), 100); got != 13 {
			t.Errorf("%d retries, want 13: the late count cleared the newer ones", got)
		}
	})
}

// A burst of retries from many requests at once takes exactly the budget.
func TestRetryBudgetUnderLoad(t *testing.T) {
	var b retryBudget
	for range 100 {
		b.first(noon)
	}
	var got atomic.Int32
	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			if b.spend(noon) {
				got.Add(1)
			}
		})
	}
	wg.Wait()
	if got.Load() != 23 {
		t.Errorf("%d retries, want 23", got.Load())
	}
}

// attemptLines returns the gateway's warnings about failed attempts.
func attemptLines(t *testing.T, logs *lockedBuffer) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, l := range logs.lines(t) {
		if l["msg"] == "upstream attempt failed" {
			found = append(found, l)
		}
	}
	return found
}

// Two failures, then success: the caller sees only the success.
func TestRetriesUntilSuccess(t *testing.T) {
	r := setup(t, options{random: half, fake: func(c *fakeupstream.Config) { c.FailFirst = 2 }})
	began := time.Now()
	resp, body := post(t, r.gatewayURL+"/v1/embeddings", nil, embedBody)
	took := time.Since(began)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"embedding"`) {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if n := r.fake.Stats().Requests; n != 3 {
		t.Errorf("the upstream got %d requests, want 3", n)
	}
	// Half of 200 ms, then half of 400 ms.
	if took < 300*time.Millisecond {
		t.Errorf("took %v; the backoff alone is 300 ms", took)
	}
	lines := attemptLines(t, r.logs)
	if len(lines) != 2 {
		t.Fatalf("%d attempt lines, want 2", len(lines))
	}
	for i, l := range lines {
		check(t, l, map[string]any{
			"level": "WARN", "attempt": float64(i + 1), "upstream": "fake",
			"reason": "503 Service Unavailable", "request_id": resp.Header.Get("X-Request-ID"),
		})
	}
	check(t, lines[0], map[string]any{"next": "retry in 100ms"})
	check(t, lines[1], map[string]any{"next": "retry in 200ms"})
	line := accessLine(t, r.logs, withID(resp.Header.Get("X-Request-ID")))
	check(t, line, map[string]any{"status": 200.0, "attempts": 3.0})
}

// After the last attempt, the caller gets the upstream's own answer.
func TestGivesUpAfterThreeAttempts(t *testing.T) {
	r := setup(t, options{random: noJitter, fake: func(c *fakeupstream.Config) {
		c.FailRate = 1
		c.FailStatus = 502
	}})
	direct, directBody := post(t, r.fakeURL+"/v1/embeddings", nil, embedBody)
	r.fake.Reset()
	via, viaBody := post(t, r.gatewayURL+"/v1/embeddings", nil, embedBody)
	if via.StatusCode != 502 || string(viaBody) != string(directBody) {
		t.Errorf("via the gateway: %d %s\ndirect: %d %s", via.StatusCode, viaBody, direct.StatusCode, directBody)
	}
	if n := r.fake.Stats().Requests; n != 3 {
		t.Errorf("the upstream got %d requests, want 3", n)
	}
	lines := attemptLines(t, r.logs)
	if len(lines) != 3 || lines[2]["next"] != "none: no attempts left" {
		t.Errorf("attempt lines: %v", lines)
	}
}

// The caller's own mistakes, and an internal error, come out the same every
// time.
func TestNotRetried(t *testing.T) {
	for _, status := range []int{400, 401, 404, 500} {
		r := setup(t, options{random: noJitter, fake: func(c *fakeupstream.Config) {
			c.FailRate = 1
			c.FailStatus = status
		}})
		if resp, _ := post(t, r.gatewayURL+"/v1/embeddings", nil, embedBody); resp.StatusCode != status {
			t.Errorf("%d: got %d", status, resp.StatusCode)
		}
		if n := r.fake.Stats().Requests; n != 1 {
			t.Errorf("%d: the upstream got %d requests, want 1", status, n)
		}
		if lines := attemptLines(t, r.logs); len(lines) != 0 {
			t.Errorf("%d: logged %v", status, lines)
		}
	}
}

// Once the first event has gone to the caller, a break is not retried: the
// caller would get a second stream spliced onto the first.
func TestBrokenStreamNotRetried(t *testing.T) {
	r := setup(t, options{random: noJitter, fake: func(c *fakeupstream.Config) { c.BreakAfter = 2 }})
	resp, body := post(t, r.gatewayURL+"/v1/chat/completions", nil, chatBody)
	events := splitBody(body)
	if resp.StatusCode != 200 || !strings.HasPrefix(events[len(events)-1], "event: error") {
		t.Errorf("got %d, ending with %q", resp.StatusCode, events[len(events)-1])
	}
	if n := r.fake.Stats().Requests; n != 1 {
		t.Errorf("the upstream got %d requests, want 1", n)
	}
}

// flaky answers with fail on its first n calls, and with a short stream
// after.
func flaky(t *testing.T, n int32, fail http.HandlerFunc) *httptest.Server {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if calls.Add(1) <= n {
			fail(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// An upstream that sends its headers and then dies has sent the caller
// nothing yet, so it is still retried.
func TestHeadersThenDeathIsRetried(t *testing.T) {
	upstream := flaky(t, 1, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		http.NewResponseController(w).Flush()
		panic(http.ErrAbortHandler)
	})
	gw, client := startGateway(t, upstream.URL, "")
	t.Cleanup(func() {
		gw.Close()
		client.CloseIdleConnections()
	})
	resp, body := post(t, gw.URL+"/v1/chat/completions", nil, chatBody)
	if resp.StatusCode != 200 || string(body) != "data: {}\n\ndata: [DONE]\n\n" {
		t.Errorf("got %d %q, want the second attempt's stream alone", resp.StatusCode, body)
	}
}

// A first attempt that runs out of time does not end the request: the
// timeout is the attempt's, not the request's.
func TestFirstByteTimeoutIsRetried(t *testing.T) {
	upstream := flaky(t, 1, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	cfg := oneUpstream(upstream.URL, "")
	for name, m := range cfg.Models {
		m.Timeouts.FirstByte = 200 * time.Millisecond
		cfg.Models[name] = m
	}
	client := NewClient()
	gw, logs := startGatewayLogged(t, cfg, client, noJitter)
	t.Cleanup(func() {
		gw.Close()
		client.CloseIdleConnections()
	})
	resp, body := post(t, gw.URL+"/v1/chat/completions", nil, chatBody)
	if resp.StatusCode != 200 || !strings.HasSuffix(string(body), "data: [DONE]\n\n") {
		t.Errorf("got %d %q", resp.StatusCode, body)
	}
	if lines := attemptLines(t, logs); len(lines) != 1 || lines[0]["reason"] != "first_byte timeout" {
		t.Errorf("attempt lines: %v", lines)
	}
	line := accessLine(t, logs, withID(resp.Header.Get("X-Request-ID")))
	check(t, line, map[string]any{"status": 200.0, "attempts": 2.0, "timeout": ""})
}

// The upstream's Retry-After is a minimum wait.
func TestRetryAfterIsWaited(t *testing.T) {
	r := setup(t, options{random: noJitter, fake: func(c *fakeupstream.Config) {
		c.FailFirst = 1
		c.FailStatus = 429
		c.RetryAfter = 1
	}})
	began := time.Now()
	resp, _ := post(t, r.gatewayURL+"/v1/embeddings", nil, embedBody)
	if took := time.Since(began); resp.StatusCode != 200 || took < time.Second {
		t.Errorf("got %d after %v, want 200 after the 1 s the upstream asked for", resp.StatusCode, took)
	}
}

// A retry that could only start after the deadline is not made; the caller
// gets the failure at once. Nor does it take from the budget: five requests
// would have used it up.
func TestNoRetryPastTheDeadline(t *testing.T) {
	r := setup(t, options{random: noJitter, fake: func(c *fakeupstream.Config) {
		c.FailRate = 1
		c.RetryAfter = 1 // with the default 503
	}})
	for range 5 {
		began := time.Now()
		resp, _ := post(t, r.gatewayURL+"/v1/embeddings", http.Header{"X-Timeout-Ms": {"500"}}, embedBody)
		if took := time.Since(began); resp.StatusCode != 503 || took > 400*time.Millisecond {
			t.Errorf("got %d after %v, want the upstream's 503 at once", resp.StatusCode, took)
		}
	}
	lines := attemptLines(t, r.logs)
	if len(lines) != 5 {
		t.Fatalf("%d attempt lines, want 5", len(lines))
	}
	for _, l := range lines {
		check(t, l, map[string]any{"next": "none: the wait would end past the deadline"})
	}
}

// A caller that leaves during the wait is not followed by another attempt.
func TestCallerLeavesDuringTheWait(t *testing.T) {
	r := setup(t, options{random: noJitter, fake: func(c *fakeupstream.Config) {
		c.FailFirst = 1
		c.RetryAfter = 5
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if resp, err := http.DefaultClient.Do(newRequest(ctx, r.gatewayURL+"/v1/embeddings", embedBody)); err == nil {
		resp.Body.Close()
		t.Fatalf("got %d, want the client to give up", resp.StatusCode)
	}
	line := accessLine(t, r.logs, func(l map[string]any) bool { return l["status"] == 499.0 })
	if total := ms(line, "total_ms"); total > 1000 {
		t.Errorf("the gateway let the request go after %v ms, not when the caller left", total)
	}
	if n := r.fake.Stats().Requests; n != 1 {
		t.Errorf("the upstream got %d requests, want 1", n)
	}
}

// When the upstream is down, everyone retries at once, and the budget keeps
// the extra load to a fifth, plus 3.
func TestBudgetCapsRetriesToADeadUpstream(t *testing.T) {
	t.Run("one after another", func(t *testing.T) {
		r := setup(t, options{random: noJitter, fake: func(c *fakeupstream.Config) { c.FailRate = 1 }})
		for range 50 {
			if resp, _ := post(t, r.gatewayURL+"/v1/embeddings", nil, embedBody); resp.StatusCode != 503 {
				t.Fatalf("got %d, want the upstream's 503", resp.StatusCode)
			}
		}
		// Each first attempt adds a fifth of a retry to the budget.
		if n := r.fake.Stats().Requests; n != 50+13 {
			t.Errorf("the upstream got %d requests, want 63: 50, a fifth more, and 3", n)
		}
	})
	t.Run("all at once", func(t *testing.T) {
		r := setup(t, options{random: noJitter, fake: func(c *fakeupstream.Config) { c.FailRate = 1 }})
		var wg sync.WaitGroup
		for range 50 {
			wg.Go(func() {
				resp, err := http.DefaultClient.Do(newRequest(context.Background(), r.gatewayURL+"/v1/embeddings", embedBody))
				if err != nil {
					t.Error(err)
					return
				}
				resp.Body.Close()
				if resp.StatusCode != 503 {
					t.Errorf("got %d, want the upstream's 503", resp.StatusCode)
				}
			})
		}
		wg.Wait()
		// Without the budget, 150. Retries made while few first attempts
		// had been counted may leave some of the 13 unspent.
		if n := r.fake.Stats().Requests; n <= 50 || n > 50+13 {
			t.Errorf("the upstream got %d requests, want more than 50, and at most 63", n)
		}
	})
}

// A failed attempt's short answer has been read to its end by the time it is
// closed, so the next attempt can use the same connection.
func TestRetriesReuseTheConnection(t *testing.T) {
	var conns atomic.Int32
	r := setup(t, options{
		random: noJitter,
		fake:   func(c *fakeupstream.Config) { c.FailFirst = 2 },
		connState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateNew {
				conns.Add(1)
			}
		},
	})
	if resp, _ := post(t, r.gatewayURL+"/v1/embeddings", nil, embedBody); resp.StatusCode != 200 {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if n := conns.Load(); n != 1 {
		t.Errorf("3 attempts, one after another, opened %d upstream connections", n)
	}
}

// A failed attempt lets go of its connection even when its body is too long
// to have been read to its end; otherwise the connection, and the upstream's
// handler, would wait on it for good.
func TestFailedAttemptsBodyIsClosed(t *testing.T) {
	gone := make(chan struct{})
	upstream := flaky(t, 1, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		chunk := make([]byte, 64<<10)
		for {
			if _, err := w.Write(chunk); err != nil {
				close(gone) // the gateway hung up
				return
			}
		}
	})
	gw, client := startGateway(t, upstream.URL, "")
	t.Cleanup(func() {
		upstream.CloseClientConnections() // first, or a failure would hang the cleanup
		gw.Close()
		client.CloseIdleConnections()
	})
	if resp, _ := post(t, gw.URL+"/v1/chat/completions", nil, chatBody); resp.StatusCode != 200 {
		t.Fatalf("got %d", resp.StatusCode)
	}
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Error("the first attempt's connection was never let go")
	}
}

// The first-byte timer can fire just as the first byte comes. The attempt
// has then lost its context, and must not be relayed as an answer.
func TestTimerFiringAsTheByteComes(t *testing.T) {
	ctx, stop := context.WithCancelCause(context.Background())
	p := &Proxy{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		// The byte is there, but by the time it is read, the timer has fired.
		body := readerFunc(func(b []byte) (int, error) {
			stop(firstByteTimeout)
			return copy(b, "{}"), io.EOF
		})
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(body)}, nil
	})}}
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://upstream/v1/embeddings", strings.NewReader(embedBody))
	resp, _, err := p.send(ctx, stop, req, time.Minute, time.Now().Add(time.Minute))
	if t2, ok := timeoutOf(ctx, err); resp != nil || !ok || t2 != firstByteTimeout {
		t.Errorf("got %v, %v; want no answer, and the first-byte timeout", resp, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(b []byte) (int, error) { return f(b) }
