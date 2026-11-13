package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// httptest.NewServer runs a real server on a free localhost port, so these
// requests go through everything: client, TCP, server, mux, handler.
func TestFetch(t *testing.T) {
	srv := httptest.NewServer(Routes(2 * time.Second))
	defer srv.Close()
	client := &http.Client{Timeout: 100 * time.Millisecond}

	t.Run("healthz", func(t *testing.T) {
		status, body, err := Fetch(context.Background(), client, srv.URL+"/healthz")
		if err != nil || status != 200 || body != "ok\n" {
			t.Errorf("Fetch() = (%d, %q, %v), want (200, %q, nil)", status, body, err, "ok\n")
		}
	})

	t.Run("server slower than the timeout", func(t *testing.T) {
		start := time.Now()
		_, _, err := Fetch(context.Background(), client, srv.URL+"/slow")
		elapsed := time.Since(start)
		t.Logf("client got: %v", err)

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want context.DeadlineExceeded", err)
		}
		if elapsed > time.Second {
			t.Errorf("gave up after %v, want about 100ms", elapsed)
		}
	})
}
