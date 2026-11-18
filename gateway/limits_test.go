package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GJH1203/sous/internal/fakeupstream"
)

var noon = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// allowed calls allow n times for caller at now, and counts the trues.
func allowed(l *callerLimits, caller string, now time.Time, n int) int {
	got := 0
	for range n {
		if ok, _ := l.allow(caller, now); ok {
			got++
		}
	}
	return got
}

func TestCallersHaveBucketsOfTheirOwn(t *testing.T) {
	l := newCallerLimits([]Caller{
		{Name: "a", RateLimit: RateLimit{Burst: 2, PerSecond: 1}},
		{Name: "b", RateLimit: RateLimit{Burst: 3, PerSecond: 1}},
	})
	if got := allowed(l, "a", noon, 2); got != 2 {
		t.Fatalf("a: %d of its burst of 2 let through", got)
	}
	if ok, wait := l.allow("a", noon); ok || wait != time.Second {
		t.Errorf("a's third request: allowed %v, wait %v; want refused, 1s", ok, wait)
	}
	if got := allowed(l, "b", noon, 4); got != 3 {
		t.Errorf("b: %d let through after a ran out, want its own burst of 3", got)
	}
}

// A full bucket is dropped once a sweep is due; one still filling is kept,
// since a new one would hand its caller a fresh burst.
func TestFullBucketsAreDropped(t *testing.T) {
	l := newCallerLimits([]Caller{
		{Name: "quick", RateLimit: RateLimit{Burst: 2, PerSecond: 1}},    // full 1 s after a request
		{Name: "slow", RateLimit: RateLimit{Burst: 2, PerSecond: 0.001}}, // 1000 s
		{Name: "other", RateLimit: RateLimit{Burst: 1, PerSecond: 1}},
	})
	l.allow("quick", noon)
	l.allow("slow", noon)
	later := noon.Add(sweepEvery)
	l.allow("other", later)
	if _, ok := l.buckets["quick"]; ok {
		t.Error("quick's bucket was kept, though it was full")
	}
	if _, ok := l.buckets["slow"]; !ok {
		t.Error("slow's bucket was dropped, though it was not full")
	}
	if got := allowed(l, "slow", later, 2); got != 1 {
		t.Errorf("slow: %d let through, want the 1 token it had left", got)
	}
	if got := allowed(l, "quick", later, 3); got != 2 {
		t.Errorf("quick: %d let through, want a whole burst of 2 from its new bucket", got)
	}
}

// Many requests at once, each reading the clock before taking the lock, so
// the times arrive out of order. Exactly the burst gets through.
func TestCallerLimitsUnderLoad(t *testing.T) {
	l := newCallerLimits([]Caller{{Name: "a", RateLimit: RateLimit{Burst: 50, PerSecond: 0.001}}})
	var got atomic.Int32
	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			if ok, _ := l.allow("a", time.Now()); ok {
				got.Add(1)
			}
		})
	}
	wg.Wait()
	if got.Load() != 50 {
		t.Errorf("%d of 200 let through, want 50", got.Load())
	}
}

func TestRateLimitedCallerGets429(t *testing.T) {
	var calls atomic.Int32
	cfg := &Config{Callers: []Caller{
		{Name: "app", APIKey: "app-key", RateLimit: RateLimit{Burst: 2, PerSecond: 0.001}},
		{Name: "other", APIKey: "other-key", RateLimit: generous},
	}}
	srv := httptest.NewServer(NewHandler(cfg, echo(&calls), slog.New(slog.DiscardHandler)))
	t.Cleanup(srv.Close)
	url := srv.URL + "/v1/embeddings"

	// A request with a wrong key never reaches the limit, so it takes no
	// token.
	if resp, _ := send(t, "POST", url, "Bearer wrong"); resp.StatusCode != 401 {
		t.Fatalf("wrong key: %d", resp.StatusCode)
	}
	for i := range 2 {
		if resp, _ := send(t, "POST", url, "Bearer app-key"); resp.StatusCode != 200 {
			t.Fatalf("request %d of the burst: %d", i+1, resp.StatusCode)
		}
	}
	resp, body := send(t, "POST", url, "Bearer app-key")
	if resp.StatusCode != 429 {
		t.Fatalf("after the burst: %d, want 429", resp.StatusCode)
	}
	// The next token is 1000 s minus a moment away; rounded up, 1000.
	if got := resp.Header.Get("Retry-After"); got != "1000" {
		t.Errorf("Retry-After: %q, want \"1000\"", got)
	}
	if e := gatewayError(t, body); e.Code != "rate_limited" || e.Type != "rate_limit_error" {
		t.Errorf("error: %+v", e)
	}
	if resp, _ := send(t, "POST", url, "Bearer other-key"); resp.StatusCode != 200 {
		t.Errorf("the other caller got %d while app was limited", resp.StatusCode)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("%d requests reached the proxy, want 3: a refused one must not", n)
	}
}

func TestModelsOnOneUpstreamShareItsSlots(t *testing.T) {
	p := NewProxy(http.DefaultClient, oneUpstream("http://127.0.0.1:1", ""), slog.New(slog.DiscardHandler))
	chat, embed := p.routes["chat"].slots, p.routes["embed"].slots
	if chat != embed {
		t.Error("chat and embed have separate slots on the same upstream")
	}
	if p.routes["chat"].budget != p.routes["embed"].budget {
		t.Error("chat and embed have separate retry budgets on the same upstream")
	}
	if cap(chat) != 100 {
		t.Errorf("%d slots, want the upstream's max_concurrent, 100", cap(chat))
	}
}

// Fifty streams at once, to an upstream that takes four: the upstream never
// has more than four, and every stream gets through.
func TestUpstreamGetsAtMostItsLimit(t *testing.T) {
	r := setup(t, options{maxConcurrent: 4, fake: func(c *fakeupstream.Config) {
		c.ChunkInterval = 10 * time.Millisecond // 50 ms a stream
	}})
	statuses := make(chan int, 50)
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			resp, err := http.DefaultClient.Do(newRequest(context.Background(), r.gatewayURL+"/v1/chat/completions", chatBody))
			if err != nil {
				statuses <- 0
				return
			}
			defer resp.Body.Close()
			// Read the stream to its end: the slot is held until then.
			body, err := io.ReadAll(resp.Body)
			if err != nil || !strings.HasSuffix(string(body), "data: [DONE]\n\n") {
				statuses <- -1
				return
			}
			statuses <- resp.StatusCode
		})
	}
	wg.Wait()
	close(statuses)
	for s := range statuses {
		if s != 200 {
			t.Errorf("a stream ended with %d (0: no response, -1: cut off)", s)
		}
	}
	if got := r.fake.Stats().MaxConcurrent; got != 4 {
		t.Errorf("the upstream had up to %d requests at once, want 4", got)
	}
}

// holdSlot starts a stream that keeps one slot until the test ends.
func holdSlot(t *testing.T, r rig) {
	t.Helper()
	hangUp := startStream(t, r.gatewayURL)
	t.Cleanup(hangUp)
}

// slowFake sends a stream's first event, then nothing for a minute.
func slowFake(c *fakeupstream.Config) {
	c.Chunks = 100
	c.ChunkInterval = time.Minute
}

func TestQueueTimeoutGets503(t *testing.T) {
	r := setup(t, options{maxConcurrent: 1, queueTimeout: 100 * time.Millisecond, fake: slowFake})
	holdSlot(t, r)
	// A deadline of its own, so that a request that never leaves the queue,
	// or gets a slot it should not have, fails the test instead of hanging it.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := http.DefaultClient.Do(newRequest(ctx, r.gatewayURL+"/v1/chat/completions", chatBody))
	if err != nil {
		t.Fatalf("no answer: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	waited := time.Since(start)
	if err != nil {
		t.Fatalf("no whole answer: %v", err)
	}
	if resp.StatusCode != 503 || gatewayError(t, body).Code != "upstream_busy" {
		t.Errorf("got %d %s, want 503 upstream_busy", resp.StatusCode, body)
	}
	if waited < 100*time.Millisecond || waited > 2*time.Second {
		t.Errorf("answered after %v, want about the queue timeout, 100ms", waited)
	}
	if n := r.fake.Stats().Requests; n != 1 {
		t.Errorf("the upstream got %d requests, want only the one holding the slot", n)
	}
}

// A caller that gives up while waiting leaves the queue then, not when the
// queue timeout comes.
func TestCallerLeavesTheQueueAtOnce(t *testing.T) {
	r := setup(t, options{maxConcurrent: 1, fake: slowFake}) // the queue timeout is 10 s
	holdSlot(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if resp, err := http.DefaultClient.Do(newRequest(ctx, r.gatewayURL+"/v1/chat/completions", chatBody)); err == nil {
		resp.Body.Close()
		t.Fatalf("got %d, want the client to give up", resp.StatusCode)
	}
	line := accessLine(t, r.logs, func(l map[string]any) bool { return l["status"] == 499.0 })
	if total := ms(line, "total_ms"); total > 1000 {
		t.Errorf("the gateway let the request go after %v ms", total)
	}
	// A caller leaving is not the upstream failing.
	for _, l := range r.logs.lines(t) {
		if l["level"] == "WARN" {
			t.Errorf("logged %v", l)
		}
	}
	if n := r.fake.Stats().Requests; n != 1 {
		t.Errorf("the upstream got %d requests, want only the one holding the slot", n)
	}
}
