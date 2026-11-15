package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Server serves the gateway and shuts it down without cutting requests off,
// as long as they finish within the grace period.
type Server struct {
	srv      *http.Server
	log      *slog.Logger
	grace    time.Duration
	draining atomic.Bool  // set once shutdown starts; /readyz reports it
	inFlight atomic.Int64 // API requests that have not finished yet
}

// NewServer serves api, with the health probes in front of it.
func NewServer(cfg *Config, api http.Handler, log *slog.Logger) *Server {
	s := &Server{log: log, grace: cfg.ShutdownGrace}
	mux := http.NewServeMux()
	// The probes stay outside the API's middleware: whatever polls them has
	// no key, and a line in the access log every few seconds would bury the
	// real requests.
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("/", s.track(api))
	s.srv = &http.Server{
		Handler: mux,
		// Limits how long a client may take to send its headers. There is no
		// WriteTimeout: it would cut long streams off.
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Serve serves on ln until ctx is done. Then it stops taking new connections,
// waits up to the grace period for the requests in flight, and cuts off the
// ones still running. It returns only once all of them are over, so the
// program must not exit before it does.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	served := make(chan error, 1)
	go func() { served <- s.srv.Serve(ln) }()
	select {
	case err := <-served:
		// Before a shutdown, Serve returns only when it cannot accept
		// connections any more.
		return err
	case <-ctx.Done():
	}

	s.draining.Store(true)
	s.log.Info("shutting down", "cause", context.Cause(ctx),
		"in_flight", s.inFlight.Load(), "grace", s.grace.String())
	// Not ctx: it is done already, and Shutdown would give up at once.
	graceCtx, cancel := context.WithTimeout(context.Background(), s.grace)
	defer cancel()
	err := s.srv.Shutdown(graceCtx)
	if errors.Is(err, context.DeadlineExceeded) {
		s.log.Warn("grace period over, cutting requests off", "cut_off", s.inFlight.Load())
		// Closing a connection cancels its request's context, and with it
		// the request to the upstream.
		err = s.srv.Close()
	}
	return err
}

// track counts the API requests in flight, so that a shutdown can tell how
// many it cut off.
func (s *Server) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.inFlight.Add(1)
		// Deferred, so that a request ended by a panic is not counted forever.
		defer s.inFlight.Add(-1)
		next.ServeHTTP(w, r)
	})
}

// healthz says the process is alive. It stays 200 during a shutdown: a
// supervisor that saw 503 here would kill the process, and with it the
// requests that the shutdown is waiting for.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "ok")
}

// readyz says whether to send the gateway new requests. It does not check the
// upstreams: with them down, the gateway still answers with an error that says
// so, which beats not being reached at all.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ok")
}
