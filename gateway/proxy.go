package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"time"
)

// maxBodyBytes caps a request body. The longest chat the local models accept
// is well under 1 MiB of JSON, so the cap only stops mistakes, and it bounds
// the memory each request can hold.
const maxBodyBytes = 4 << 20

// NewClient returns the one HTTP client the whole gateway shares. Its
// Transport owns the connection pool, so a new client per request would open
// a new connection per request.
func NewClient() *http.Client {
	return newClient((&net.Dialer{}).DialContext)
}

// newClient takes the dial function so that a test can stand in a network
// where connecting is slow, which no server on localhost can be made to be.
func newClient(dial dialFunc) *http.Client {
	return &http.Client{
		// No timeouts here: they differ by model, so each request brings its
		// own. Client.Timeout would also cover reading the body, and cut long
		// streams off.
		Transport: &http.Transport{
			// TLS, to a cloud upstream, has no limit of its own: it is part of
			// the wait for the first byte.
			DialContext: dialWithin(dial),
			// The default keeps only 2 idle connections per upstream; past 2
			// concurrent requests, the rest are closed and dialed again.
			MaxIdleConnsPerHost: 32,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// Proxy forwards each request to the upstream that serves the model it names,
// and copies the answer back.
type Proxy struct {
	client *http.Client
	// routes is keyed by logical model name. It is never written after
	// NewProxy, so concurrent requests can read it without a lock.
	routes map[string]route
	log    *slog.Logger
	// random returns a number in [0, 1) for the backoff. A field, so that a
	// test can make the waits exact.
	random func() float64
}

// route is where the requests for one logical model go.
type route struct {
	upstream string // the upstream's name, for the log
	baseURL  string
	apiKey   string // the gateway's own key for the upstream; empty for none
	model    string // the upstream's name for the model
	// slots and budget belong to the upstream, not the model: every model
	// the upstream serves shares them.
	slots        slots
	budget       *retryBudget
	queueTimeout time.Duration
	timeouts     Timeouts
}

// NewProxy returns a Proxy that routes by cfg, which must have come from
// LoadConfig: it checks that every model's upstream exists.
func NewProxy(client *http.Client, cfg *Config, log *slog.Logger) *Proxy {
	upstreams := make(map[string]Upstream)
	slotsOf := make(map[string]slots)
	budgetOf := make(map[string]*retryBudget)
	for _, u := range cfg.Upstreams {
		upstreams[u.Name] = u
		slotsOf[u.Name] = make(slots, u.MaxConcurrent)
		budgetOf[u.Name] = &retryBudget{}
	}
	routes := make(map[string]route)
	for name, m := range cfg.Models {
		u := upstreams[m.Upstream]
		routes[name] = route{upstream: u.Name, baseURL: u.BaseURL, apiKey: u.APIKey, model: m.Model,
			slots: slotsOf[u.Name], budget: budgetOf[u.Name], queueTimeout: u.QueueTimeout, timeouts: m.Timeouts}
	}
	return &Proxy{client: client, routes: routes, log: log, random: rand.Float64}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	arrived := time.Now()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large",
				fmt.Sprintf("the request body is larger than %d bytes", tooBig.Limit))
			return
		}
		writeError(w, http.StatusBadRequest, "unreadable_body", "the request body could not be read")
		return
	}

	// Only "model" is decoded. The other values stay raw JSON, so fields the
	// gateway knows nothing about reach the upstream with the same meaning.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "the request body is not a JSON object")
		return
	}
	var model string
	if err := json.Unmarshal(fields["model"], &model); err != nil || model == "" {
		writeError(w, http.StatusBadRequest, "missing_model", "the request body has no model string")
		return
	}
	info := infoFrom(r.Context())
	info.model = model
	rt, ok := p.routes[model]
	if !ok {
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("the model %q doesn't exist", model))
		return
	}
	info.upstream = rt.upstream
	// Marshal cannot fail on a string, nor on raw values that just parsed.
	fields["model"], _ = json.Marshal(rt.model)
	body, _ = json.Marshal(fields)

	due, err := deadline(r, arrived, rt.timeouts.Total)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_timeout", err.Error())
		return
	}
	// Built on the inbound request's context: when the caller goes away, the
	// upstream request is cancelled with it, and the model stops generating.
	// The deadline is set once and never moved, so the queue, every attempt,
	// the waits between them and the whole stream all count against it.
	ctx, cancel := context.WithDeadlineCause(r.Context(), due, totalTimeout)
	defer cancel()
	ctx = context.WithValue(ctx, connectKey{}, rt.timeouts.Connect)

	// Each attempt sends a copy of this. The body is a bytes.Reader, so the
	// request can make a new one for every copy.
	req, err := http.NewRequestWithContext(ctx, r.Method, rt.baseURL+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		p.log.Error("building the upstream request", "request_id", requestIDFrom(r.Context()), "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "the gateway could not build the upstream request")
		return
	}
	copyRequestHeader(req.Header, r.Header)
	if rt.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+rt.apiKey)
	}
	if id := requestIDFrom(r.Context()); id != "" {
		// So that the upstream's log lines can be matched with the gateway's.
		req.Header.Set("X-Request-ID", id)
	}

	for n := 1; ; n++ {
		info.attempts = n
		wait, again := p.attempt(ctx, w, r, rt, req, due, n)
		if !again {
			return
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			p.fail(ctx, w, r, context.Cause(ctx)) // the caller left, or the deadline came
			return
		}
	}
}

// attempt makes attempt n at the upstream, and answers the caller, unless it
// failed in a way that another attempt might not. Then it answers no one, and
// returns how long to wait before the next attempt, and true.
func (p *Proxy) attempt(ctx context.Context, w http.ResponseWriter, r *http.Request, rt route, req *http.Request, due time.Time, n int) (time.Duration, bool) {
	if err := rt.slots.acquire(ctx, rt.queueTimeout); err != nil {
		if errors.Is(err, errNoSlot) {
			writeError(w, http.StatusServiceUnavailable, "upstream_busy",
				fmt.Sprintf("the upstream is busy, and no slot came free within %v", rt.queueTimeout))
			return 0, false
		}
		p.fail(ctx, w, r, err) // the caller left, or the deadline came
		return 0, false
	}
	// Deferred, so the slot is held until the whole answer is relayed: a
	// stream keeps its slot in the engine until its last token. A failed
	// attempt gives it back before the wait.
	defer rt.slots.release()
	if n == 1 {
		rt.budget.first(time.Now())
	}

	// The attempt's own context: the first-byte and idle timers end the
	// attempt, each with its own cause, and the request may go on.
	actx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	resp, first, err := p.send(actx, stop, req, rt.timeouts.FirstByte, due)
	if resp != nil {
		// net/http asks for every body to be closed. Cancelling the
		// attempt's context, deferred above, would free the connection too,
		// but only as a side effect.
		defer resp.Body.Close()
	}

	var reason string
	var after time.Duration // the least wait the upstream asks for
	switch {
	case err == nil && !retryable(resp.StatusCode):
		p.relay(actx, stop, w, r, resp, first, rt.timeouts.Idle)
		return 0, false
	case err == nil:
		reason = resp.Status
		after = retryAfter(resp.Header, time.Now())
	case ctx.Err() != nil:
		p.fail(actx, w, r, err) // the caller left, or the deadline came
		return 0, false
	default:
		reason = err.Error()
		if t, ok := timeoutOf(actx, err); ok {
			reason = t.Error()
		}
	}

	wait := max(backoff(n, p.random()), after)
	var why string
	switch {
	case n == maxAttempts:
		why = "no attempts left"
	case time.Now().Add(wait).After(due):
		why = "the wait would end past the deadline"
	// Last, since it takes from the budget.
	case !rt.budget.spend(time.Now()):
		why = "over the upstream's retry budget"
	}
	if why == "" {
		p.log.Warn("upstream attempt failed", "request_id", requestIDFrom(r.Context()), "attempt", n,
			"upstream", rt.upstream, "reason", reason, "next", "retry in "+wait.String())
		return wait, true
	}
	p.log.Warn("upstream attempt failed", "request_id", requestIDFrom(r.Context()), "attempt", n,
		"upstream", rt.upstream, "reason", reason, "next", "none: "+why)
	// The last failure goes to the caller as it is.
	if err == nil {
		p.relay(actx, stop, w, r, resp, first, rt.timeouts.Idle)
	} else {
		p.fail(actx, w, r, err)
	}
	return 0, false
}

// send makes one attempt: it sends a copy of req, with ctx, and waits for the
// first byte of the answer's body. ctx is the attempt's own, and stop cancels
// it. The answer comes back with its first bytes read into first.
func (p *Proxy) send(ctx context.Context, stop context.CancelCauseFunc, req *http.Request, firstByte time.Duration, due time.Time) (*http.Response, *bufio.Reader, error) {
	// Clone copies the headers but shares the body, which the attempt before
	// has read to its end. GetBody makes a new one from the same bytes.
	req = req.Clone(ctx)
	req.Body, _ = req.GetBody() // cannot fail on a bytes.Reader
	// The time left, rounded up, so that a server that reads it can drop
	// work no one will wait for.
	left := (time.Until(due) + time.Millisecond - 1) / time.Millisecond
	req.Header.Set("X-Timeout-Ms", strconv.FormatInt(int64(left), 10))

	// Started before the request is sent, so it covers connecting too.
	timer := time.AfterFunc(firstByte, func() { stop(firstByteTimeout) })
	defer timer.Stop()
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	// Wait for the first byte of the body before answering: until something
	// has been written, the request can still be retried, or end in a plain
	// 504. Peek reads it without taking it out of the body.
	first := bufio.NewReader(resp.Body)
	_, err = first.Peek(1)
	timer.Stop()
	if err == io.EOF {
		err = nil // an empty body is an answer too
	}
	if err == nil {
		// Nil, unless the timer fired just as the byte came.
		err = context.Cause(ctx)
	}
	if err != nil {
		resp.Body.Close()
		return nil, nil, err
	}
	return resp, first, nil
}

// relay copies an answer to the caller. Once it has written anything, the
// request cannot be retried: the caller would get a second answer spliced
// onto the first.
func (p *Proxy) relay(ctx context.Context, stop context.CancelCauseFunc, w http.ResponseWriter, r *http.Request, resp *http.Response, first *bufio.Reader, idle time.Duration) {
	copyResponseHeader(w.Header(), resp.Header)
	if id := requestIDFrom(r.Context()); id != "" {
		// An upstream may send an ID of its own; the caller gets only ours.
		w.Header().Set("X-Request-ID", id)
	}
	w.WriteHeader(resp.StatusCode)
	info := infoFrom(r.Context())
	if isEventStream(resp.Header) {
		info.stream = true
		p.stream(ctx, w, r, first, idle, stop)
		return
	}
	if _, err := io.Copy(w, first); err != nil {
		// The status has been sent; all the caller can see is a cut-off body.
		if t, ok := timeoutOf(ctx, err); ok {
			info.timeout = string(t)
		}
		p.log.Warn("copying the upstream response", "request_id", requestIDFrom(r.Context()), "err", err)
	}
}

// fail answers for a request that got nothing from the upstream: no
// response, or no first byte of its body.
func (p *Proxy) fail(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // the caller left first; there is no one to answer
	}
	if t, ok := timeoutOf(ctx, err); ok {
		infoFrom(r.Context()).timeout = string(t)
		writeError(w, http.StatusGatewayTimeout, t.code(), "the upstream took too long: the "+t.Error()+" ran out")
		return
	}
	// The error contains the upstream's address, so it goes only to the log,
	// in the attempt's line.
	writeError(w, http.StatusBadGateway, "upstream_error", "the upstream could not be reached")
}
