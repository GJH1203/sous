// Package fakeupstream is a fake OpenAI-compatible model server whose latency
// and failures can be switched on at will. Tests run it in-process with
// httptest; cmd/fakeupstream runs it as a program.
//
// Model endpoints, shaped like llama-server's:
//
//	POST /v1/chat/completions   streaming or not, with usage
//	POST /v1/embeddings         deterministic vectors derived from the input
//	GET  /health                503 while "loading", then 200
//
// Control endpoints:
//
//	GET  /_stats    requests, concurrency and tokens since start or reset
//	POST /_reset    zero the counters and restart the failure sequence
//	GET  /_config   the current Config
//	POST /_config   merge JSON into the Config, e.g. {"fail_rate": 0.5}
//	GET  /_last     the last model request: method, path, headers, body
package fakeupstream

import (
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// maxBodyBytes is larger than any limit a proxy in front would set, so that
// the proxy's own limit is the one a test runs into.
const maxBodyBytes = 16 << 20

// Server is the fake. It is an http.Handler.
type Server struct {
	mux     *http.ServeMux
	started time.Time
	quit    chan struct{}
	once    sync.Once

	mu    sync.Mutex
	cfg   Config
	rng   *rand.Rand
	stats Stats
	last  Request
}

// Stats counts model requests since start or the last reset.
type Stats struct {
	Requests         int64 `json:"requests"`
	Concurrent       int   `json:"concurrent"`
	MaxConcurrent    int   `json:"max_concurrent"`
	InjectedFailures int64 `json:"injected_failures"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// Request is what the fake received in a model request, for tests that check
// what a proxy forwarded.
type Request struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// New returns a fake that behaves as cfg says.
func New(cfg Config) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	s := &Server{
		started: time.Now(),
		quit:    make(chan struct{}),
		cfg:     cfg,
		rng:     newRand(cfg.Seed),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.model(s.chat))
	mux.HandleFunc("POST /v1/embeddings", s.model(s.embeddings))
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /_stats", s.getStats)
	mux.HandleFunc("POST /_reset", s.postReset)
	mux.HandleFunc("GET /_config", s.getConfig)
	mux.HandleFunc("POST /_config", s.postConfig)
	mux.HandleFunc("GET /_last", s.getLast)
	// llama-server answers 404 for unknown paths and for wrong methods alike.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "File Not Found", "not_found_error")
	})
	s.mux = mux
	return s, nil
}

func newRand(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, seed))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Fake-Upstream", s.Config().Name)
	s.mux.ServeHTTP(w, r)
}

// Close releases requests that Hang, Delay or ChunkInterval are holding,
// cutting their connections. Call it before closing an httptest.Server,
// whose Close waits for every running request.
func (s *Server) Close() {
	s.once.Do(func() { close(s.quit) })
}

// Config returns the current config.
func (s *Server) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// Update changes the config through f, which sees the current values. A
// result that fails validation is rejected and the old config kept.
func (s *Server) Update(f func(*Config)) error {
	_, err := s.update(func(c *Config) error {
		f(c)
		return nil
	})
	return err
}

func (s *Server) update(f func(*Config) error) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cfg
	if err := f(&next); err != nil {
		return s.cfg, err
	}
	if err := next.validate(); err != nil {
		return s.cfg, err
	}
	if next.Seed != s.cfg.Seed {
		s.rng = newRand(next.Seed)
	}
	s.cfg = next
	return next, nil
}

// Stats returns the counters.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Reset zeroes the counters and restarts the FailFirst and FailRate
// sequences. Requests still running stay counted in Concurrent, which
// would otherwise go negative when they finish.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats = Stats{Concurrent: s.stats.Concurrent, MaxConcurrent: s.stats.Concurrent}
	s.rng = newRand(s.cfg.Seed)
}

// Last returns the last model request received.
func (s *Server) Last() Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := s.last
	last.Header = last.Header.Clone()
	return last
}

// call is one model request's view of the fake, fixed when it arrives so
// that a config change does not affect requests already running.
type call struct {
	cfg  Config
	seq  int64
	fail bool
}

// begin counts a model request in and decides whether it will fail.
func (s *Server) begin(r *http.Request, body []byte) call {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Requests++
	s.stats.Concurrent++
	s.stats.MaxConcurrent = max(s.stats.MaxConcurrent, s.stats.Concurrent)
	s.last = Request{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body}

	c := call{cfg: s.cfg, seq: s.stats.Requests}
	c.fail = c.seq <= int64(c.cfg.FailFirst)
	// Draw only when FailRate is on, so turning it on later starts the
	// sequence from the seed rather than from wherever it had got to.
	if !c.fail && c.cfg.FailRate > 0 {
		c.fail = s.rng.Float64() < c.cfg.FailRate
	}
	return c
}

func (s *Server) end() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Concurrent--
}

func (s *Server) countFailure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.InjectedFailures++
}

func (s *Server) countTokens(prompt, completion int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.PromptTokens += int64(prompt)
	s.stats.CompletionTokens += int64(completion)
}

func (s *Server) loaded(cfg Config) bool {
	return time.Since(s.started) >= cfg.LoadTime
}

// pause waits for d. If the caller goes away or the fake is closed first, it
// aborts the response instead of returning.
func (s *Server) pause(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
		abort()
	case <-s.quit:
		abort()
	}
}

// abort cuts the connection mid-response. The server recovers this panic
// without logging it.
func abort() {
	panic(http.ErrAbortHandler)
}

type modelHandler func(w http.ResponseWriter, r *http.Request, c call, body []byte)

// model wraps a model endpoint with the faults, in the order a real server
// would hit them: still loading, stuck, slow, failing, and only then the
// caller's own mistakes.
func (s *Server) model(h modelHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		c := s.begin(r, body)
		defer s.end()

		if !s.loaded(c.cfg) {
			writeError(w, http.StatusServiceUnavailable, "Loading model", "unavailable_error")
			return
		}
		if c.cfg.Hang {
			select {
			case <-r.Context().Done():
			case <-s.quit:
			}
			abort()
		}
		s.pause(r.Context(), c.cfg.Delay)
		if c.fail {
			s.countFailure()
			if c.cfg.RetryAfter > 0 && (c.cfg.FailStatus == 429 || c.cfg.FailStatus == 503) {
				w.Header().Set("Retry-After", strconv.Itoa(c.cfg.RetryAfter))
			}
			writeError(w, c.cfg.FailStatus, "injected failure", errorType(c.cfg.FailStatus))
			return
		}
		if readErr != nil {
			writeError(w, http.StatusBadRequest, "could not read the request body: "+readErr.Error(), "invalid_request_error")
			return
		}
		h(w, r, c, body)
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if !s.loaded(s.Config()) {
		writeError(w, http.StatusServiceUnavailable, "Loading model", "unavailable_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) getStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Stats())
}

func (s *Server) postReset(w http.ResponseWriter, r *http.Request) {
	s.Reset()
	writeJSON(w, http.StatusOK, s.Stats())
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Config())
}

func (s *Server) postConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the request body: "+err.Error(), "invalid_request_error")
		return
	}
	cfg, err := s.update(func(c *Config) error { return json.Unmarshal(body, c) })
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) getLast(w http.ResponseWriter, r *http.Request) {
	last := s.Last()
	// Show a JSON body as JSON, so its fields are readable in curl output.
	var body any = string(last.Body)
	if json.Valid(last.Body) {
		body = json.RawMessage(last.Body)
	}
	writeJSON(w, http.StatusOK, struct {
		Method string      `json:"method"`
		Path   string      `json:"path"`
		Header http.Header `json:"header"`
		Body   any         `json:"body"`
	}{last.Method, last.Path, last.Header, body})
}
