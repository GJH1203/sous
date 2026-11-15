package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// serving is a Server running in the background on a port of its own.
type serving struct {
	s    *Server
	url  string
	logs *lockedBuffer
	stop context.CancelFunc // starts the shutdown, as a signal would
	done chan struct{}      // closed once Serve has returned
	err  error              // what Serve returned; read it once done is closed
}

// serve runs NewServer with api, the given grace period and the test caller.
func serve(t *testing.T, api http.Handler, grace time.Duration) *serving {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewJSONHandler(io.MultiWriter(t.Output(), logs), nil))
	cfg := &Config{ShutdownGrace: grace, Callers: []Caller{{Name: "test", APIKey: testKey}}}
	ctx, stop := context.WithCancel(context.Background())
	sv := &serving{
		s:    NewServer(cfg, api, logger),
		url:  "http://" + ln.Addr().String(),
		logs: logs,
		stop: stop,
		done: make(chan struct{}),
	}
	go func() {
		sv.err = sv.s.Serve(ctx, ln)
		close(sv.done)
	}()
	t.Cleanup(func() {
		stop()
		sv.s.srv.Close() // in case a test left a request hanging
		// Serve logs to the test's output, which must not be written to once
		// the test is over.
		<-sv.done
	})
	return sv
}

// returned waits for Serve to return, and fails the test if it takes longer
// than within.
func (sv *serving) returned(t *testing.T, within time.Duration) error {
	t.Helper()
	select {
	case <-sv.done:
		return sv.err
	case <-time.After(within):
		t.Fatalf("Serve has not returned after %v", within)
		return nil
	}
}

// probe asks the server's handler directly, so it still answers after the
// listener has closed.
func (sv *serving) probe(path string) (int, string) {
	// A deadline, in case the path reaches an API that holds requests.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	sv.s.srv.Handler.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, "GET", path, nil))
	return rec.Code, rec.Body.String()
}

// logLine returns the first log line with the given message, or nil.
func (sv *serving) logLine(t *testing.T, msg string) map[string]any {
	t.Helper()
	for _, line := range sv.logs.lines(t) {
		if line["msg"] == msg {
			return line
		}
	}
	return nil
}

// held is an API that holds every request until release is closed, or until
// the request's context ends. started gets a value as each request comes in.
type held struct {
	started  chan struct{}
	release  chan struct{}
	canceled atomic.Int32 // requests whose context ended first
}

func newHeld() *held {
	return &held{started: make(chan struct{}, 10), release: make(chan struct{})}
}

func (h *held) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.started <- struct{}{}
	select {
	case <-h.release:
		io.WriteString(w, "done")
	case <-r.Context().Done():
		h.canceled.Add(1)
	}
}

// recv waits for a value from ch, but not forever: a server that never lets
// go of a request should fail the test, not hang it.
func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("nothing within 3s")
		panic("unreachable")
	}
}

// get sends a GET in the background and returns a channel with its body, or
// with the error that ended it.
func get(url string) <-chan string {
	out := make(chan string, 1)
	go func() {
		resp, err := http.Get(url)
		if err != nil {
			out <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			out <- "error: " + err.Error()
			return
		}
		out <- string(b)
	}()
	return out
}

func TestProbes(t *testing.T) {
	var calls atomic.Int32
	accessLog := &lockedBuffer{}
	api := NewHandler(&Config{Callers: []Caller{{Name: "test", APIKey: testKey}}}, echo(&calls),
		slog.New(slog.NewJSONHandler(accessLog, nil)))
	sv := serve(t, api, time.Second)
	for _, path := range []string{"/healthz", "/readyz"} {
		// No key: whatever polls them has none.
		resp, body := send(t, "GET", sv.url+path, "")
		if resp.StatusCode != 200 || string(body) != "ok\n" {
			t.Errorf("GET %s: %d %q, want 200 \"ok\\n\"", path, resp.StatusCode, body)
		}
	}
	// Everything else still goes through the API's middleware.
	for _, tt := range []struct{ method, path string }{{"POST", "/healthz"}, {"GET", "/v1/models"}} {
		if resp, _ := send(t, tt.method, sv.url+tt.path, ""); resp.StatusCode != 401 {
			t.Errorf("%s %s without a key: %d, want 401", tt.method, tt.path, resp.StatusCode)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("the API was called %d times", calls.Load())
	}
	// Only the two 401s, not the probes.
	if lines := accessLog.lines(t); len(lines) != 2 {
		t.Errorf("%d access log lines, want 2: %v", len(lines), lines)
	}
}

func TestShutdownLetsRequestsFinish(t *testing.T) {
	api := newHeld()
	sv := serve(t, api, 5*time.Second)
	reply := get(sv.url + "/v1/chat/completions")
	recv(t, api.started)

	sv.stop()
	waitFor(t, time.Second, "/readyz to turn 503", func() bool {
		code, _ := sv.probe("/readyz")
		return code == 503
	})
	if code, _ := sv.probe("/healthz"); code != 200 {
		t.Errorf("/healthz during shutdown: %d, want 200", code)
	}
	waitFor(t, time.Second, "new connections to be refused", func() bool {
		conn, err := net.Dial("tcp", strings.TrimPrefix(sv.url, "http://"))
		if err == nil {
			conn.Close()
		}
		return err != nil
	})
	select {
	case <-sv.done:
		t.Fatalf("Serve returned %v with a request in flight", sv.err)
	case <-time.After(200 * time.Millisecond):
	}

	close(api.release)
	if got := recv(t, reply); got != "done" {
		t.Errorf("the request in flight got %q, want \"done\"", got)
	}
	if err := sv.returned(t, 2*time.Second); err != nil {
		t.Errorf("Serve: %v", err)
	}
	line := sv.logLine(t, "shutting down")
	if line == nil || line["in_flight"] != 1.0 || line["grace"] != "5s" || line["cause"] != "context canceled" {
		t.Errorf("shutdown line: %v", line)
	}
	if line := sv.logLine(t, "grace period over, cutting requests off"); line != nil {
		t.Errorf("cut requests off: %v", line)
	}
}

func TestShutdownCutsOffAfterGrace(t *testing.T) {
	api := newHeld()
	sv := serve(t, api, 300*time.Millisecond)
	reply := get(sv.url + "/v1/chat/completions")
	recv(t, api.started)

	start := time.Now()
	sv.stop()
	if err := sv.returned(t, 2*time.Second); err != nil {
		t.Errorf("Serve: %v", err)
	}
	if took := time.Since(start); took < 300*time.Millisecond {
		t.Errorf("Serve returned after %v, before the grace period was over", took)
	}
	if got := recv(t, reply); !strings.HasPrefix(got, "error: ") {
		t.Errorf("the request in flight got %q, want an error", got)
	}
	waitFor(t, time.Second, "the request's context to end", func() bool { return api.canceled.Load() == 1 })
	line := sv.logLine(t, "grace period over, cutting requests off")
	if line == nil || line["cut_off"] != 1.0 || line["level"] != "WARN" {
		t.Errorf("cut-off line: %v", line)
	}
}

func TestShutdownWithNothingInFlight(t *testing.T) {
	sv := serve(t, newHeld(), time.Minute)
	sv.stop()
	// Not a minute: the grace period is an upper limit, not a wait.
	if err := sv.returned(t, time.Second); err != nil {
		t.Errorf("Serve: %v", err)
	}
	if line := sv.logLine(t, "shutting down"); line == nil || line["in_flight"] != 0.0 {
		t.Errorf("shutdown line: %v", line)
	}
}

func TestServeFailsWithoutListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	s := NewServer(&Config{ShutdownGrace: time.Second}, newHeld(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	done := make(chan error, 1)
	// The context never ends: Serve must return on its own.
	go func() { done <- s.Serve(context.Background(), ln) }()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v, want the listener's error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return")
	}
}

// A request that ends in a panic must not stay counted, or every shutdown
// would report it as cut off.
func TestInFlightAfterPanic(t *testing.T) {
	sv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}), time.Second)
	if got := recv(t, get(sv.url+"/v1/chat/completions")); !strings.HasPrefix(got, "error: ") {
		t.Fatalf("got %q, want the connection cut", got)
	}
	if n := sv.s.inFlight.Load(); n != 0 {
		t.Errorf("%d requests in flight, want 0", n)
	}
}
