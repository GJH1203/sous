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
}

// route is where the requests for one logical model go.
type route struct {
	upstream string // the upstream's name, for the log
	baseURL  string
	apiKey   string // the gateway's own key for the upstream; empty for none
	model    string // the upstream's name for the model
	// slots belongs to the upstream, not the model: every model the
	// upstream serves shares it.
	slots        slots
	queueTimeout time.Duration
	timeouts     Timeouts
}

// NewProxy returns a Proxy that routes by cfg, which must have come from
// LoadConfig: it checks that every model's upstream exists.
func NewProxy(client *http.Client, cfg *Config, log *slog.Logger) *Proxy {
	upstreams := make(map[string]Upstream)
	slotsOf := make(map[string]slots)
	for _, u := range cfg.Upstreams {
		upstreams[u.Name] = u
		slotsOf[u.Name] = make(slots, u.MaxConcurrent)
	}
	routes := make(map[string]route)
	for name, m := range cfg.Models {
		u := upstreams[m.Upstream]
		routes[name] = route{upstream: u.Name, baseURL: u.BaseURL, apiKey: u.APIKey, model: m.Model,
			slots: slotsOf[u.Name], queueTimeout: u.QueueTimeout, timeouts: m.Timeouts}
	}
	return &Proxy{client: client, routes: routes, log: log}
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
	// The deadline is set once and never moved, so the queue, the upstream
	// and the whole stream all count against it.
	ctx, cancel := context.WithDeadlineCause(r.Context(), due, totalTimeout)
	defer cancel()
	// The first-byte and idle timers cancel this, each with its own cause.
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	ctx = context.WithValue(ctx, connectKey{}, rt.timeouts.Connect)

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
	id := requestIDFrom(r.Context())
	if id != "" {
		// So that the upstream's log lines can be matched with the gateway's.
		req.Header.Set("X-Request-ID", id)
	}

	if err := rt.slots.acquire(ctx, rt.queueTimeout); err != nil {
		if errors.Is(err, errNoSlot) {
			writeError(w, http.StatusServiceUnavailable, "upstream_busy",
				fmt.Sprintf("the upstream is busy, and no slot came free within %v", rt.queueTimeout))
			return
		}
		p.fail(ctx, w, r, err) // the caller left, or the deadline came
		return
	}
	// Deferred, so the slot is held until the whole answer is relayed: a
	// stream keeps its slot in the engine until its last token.
	defer rt.slots.release()

	// The time left after queueing, rounded up, so that a server that reads
	// it can drop work no one will wait for.
	left := (time.Until(due) + time.Millisecond - 1) / time.Millisecond
	req.Header.Set("X-Timeout-Ms", strconv.FormatInt(int64(left), 10))

	// Started before the request is sent, so it covers connecting too.
	timer := time.AfterFunc(rt.timeouts.FirstByte, func() { stop(firstByteTimeout) })
	defer timer.Stop()
	resp, err := p.client.Do(req)
	if err != nil {
		p.fail(ctx, w, r, err)
		return
	}
	// A body that is neither read to the end nor closed keeps its connection
	// out of the pool for good.
	defer resp.Body.Close()

	// Wait for the first byte of the body before answering: until something
	// has been written, a timeout can still be a plain 504. Peek reads it
	// without taking it out of the body.
	first := bufio.NewReader(resp.Body)
	_, err = first.Peek(1)
	timer.Stop()
	// An empty body ends with EOF at once, and that is an answer too. The
	// timer may have fired just as the byte came, so ctx is checked as well.
	if (err != nil && err != io.EOF) || ctx.Err() != nil {
		p.fail(ctx, w, r, err)
		return
	}

	copyResponseHeader(w.Header(), resp.Header)
	if id != "" {
		// An upstream may send an ID of its own; the caller gets only ours.
		w.Header().Set("X-Request-ID", id)
	}
	w.WriteHeader(resp.StatusCode)
	if isEventStream(resp.Header) {
		info.stream = true
		p.stream(ctx, w, r, first, rt.timeouts.Idle, stop)
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
	// The error contains the upstream's address, so it goes to the log, not
	// to the caller.
	p.log.Warn("upstream request failed", "request_id", requestIDFrom(r.Context()), "err", err)
	writeError(w, http.StatusBadGateway, "upstream_error", "the upstream could not be reached")
}
