package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// maxBodyBytes caps a request body. The longest chat the local models accept
// is well under 1 MiB of JSON, so the cap only stops mistakes, and it bounds
// the memory each request can hold.
const maxBodyBytes = 4 << 20

// NewClient returns the one HTTP client the whole gateway shares. Its
// Transport owns the connection pool, so a new client per request would open
// a new connection per request.
func NewClient(headerTimeout time.Duration) *http.Client {
	return &http.Client{
		// No Client.Timeout: it covers reading the body too, so it would cut
		// long streams off.
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout: 5 * time.Second,
			// How long to wait for the status line and headers once the
			// request is sent.
			ResponseHeaderTimeout: headerTimeout,
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
			slots: slotsOf[u.Name], queueTimeout: u.QueueTimeout}
	}
	return &Proxy{client: client, routes: routes, log: log}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	// The inbound request's context: when the caller goes away, the upstream
	// request is cancelled with it, and the model stops generating.
	req, err := http.NewRequestWithContext(r.Context(), r.Method, rt.baseURL+r.URL.Path, bytes.NewReader(body))
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

	if err := rt.slots.acquire(r.Context(), rt.queueTimeout); err != nil {
		if errors.Is(err, errNoSlot) {
			writeError(w, http.StatusServiceUnavailable, "upstream_busy",
				fmt.Sprintf("the upstream is busy, and no slot came free within %v", rt.queueTimeout))
		}
		return // otherwise the caller left while waiting; there is no one to answer
	}
	// Deferred, so the slot is held until the whole answer is relayed: a
	// stream keeps its slot in the engine until its last token.
	defer rt.slots.release()

	resp, err := p.client.Do(req)
	if err != nil {
		p.fail(w, r, err)
		return
	}
	// A body that is neither read to the end nor closed keeps its connection
	// out of the pool for good.
	defer resp.Body.Close()

	copyResponseHeader(w.Header(), resp.Header)
	if id != "" {
		// An upstream may send an ID of its own; the caller gets only ours.
		w.Header().Set("X-Request-ID", id)
	}
	w.WriteHeader(resp.StatusCode)
	if isEventStream(resp.Header) {
		info.stream = true
		p.stream(w, r, resp.Body)
		return
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		// The status has been sent; all the caller can see is a cut-off body.
		p.log.Warn("copying the upstream response", "request_id", requestIDFrom(r.Context()), "err", err)
	}
}

// fail answers for an upstream call that got no response at all.
func (p *Proxy) fail(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // the caller left first; there is no one to answer
	}
	// The error contains the upstream's address, so it goes to the log, not
	// to the caller.
	p.log.Warn("upstream request failed", "request_id", requestIDFrom(r.Context()), "err", err)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		writeError(w, http.StatusGatewayTimeout, "upstream_timeout", "the upstream did not answer in time")
		return
	}
	writeError(w, http.StatusBadGateway, "upstream_error", "the upstream could not be reached")
}
