package web

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// When the client gives up, the handler should stop too instead of working
// for nobody. srv.Close waits for running handlers, so its duration shows
// whether /slow noticed.
func TestSlowHandlerStopsWhenClientGivesUp(t *testing.T) {
	srv := httptest.NewServer(Routes(2 * time.Second))
	client := &http.Client{Timeout: 50 * time.Millisecond}
	if _, _, err := Fetch(context.Background(), client, srv.URL+"/slow"); err == nil {
		t.Fatal("Fetch succeeded, want a timeout")
	}

	start := time.Now()
	srv.Close()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("server took %v to close, want the handler already gone", elapsed)
	}
}

// The zero http.Client never gives up by itself. A ctx deadline is the other
// way to bound the wait, and it works only if Fetch passes ctx down.
func TestFetchHonorsContextDeadline(t *testing.T) {
	srv := httptest.NewServer(Routes(2 * time.Second))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := Fetch(ctx, &http.Client{}, srv.URL+"/slow")

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("gave up after %v, want about 50ms", elapsed)
	}
}

// Keys sometimes travel in the query string; the access log must not.
func TestLoggingLeavesOutQuery(t *testing.T) {
	var logs bytes.Buffer
	h := Logging(slog.New(slog.NewJSONHandler(&logs, nil)), Routes(time.Hour))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz?key=sk-secret", nil))

	if strings.Contains(logs.String(), "sk-secret") {
		t.Errorf("log line leaks the query string: %s", logs.String())
	}
}
