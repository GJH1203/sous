package gateway

import (
	"bytes"
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

// Proxy forwards every request to one upstream and copies the answer back.
type Proxy struct {
	client   *http.Client
	upstream string // base URL, such as http://127.0.0.1:8083
	apiKey   string // the gateway's own key for the upstream; empty for none
	log      *slog.Logger
}

// NewProxy returns a Proxy that sends requests to upstream with client.
func NewProxy(client *http.Client, upstream, apiKey string, log *slog.Logger) *Proxy {
	return &Proxy{client: client, upstream: upstream, apiKey: apiKey, log: log}
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

	// The inbound request's context: when the caller goes away, the upstream
	// request is cancelled with it, and the model stops generating.
	req, err := http.NewRequestWithContext(r.Context(), r.Method, p.upstream+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		p.log.Error("building the upstream request", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "the gateway could not build the upstream request")
		return
	}
	copyRequestHeader(req.Header, r.Header)
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		p.fail(w, r, err)
		return
	}
	// A body that is neither read to the end nor closed keeps its connection
	// out of the pool for good.
	defer resp.Body.Close()

	copyResponseHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if isEventStream(resp.Header) {
		p.stream(w, r, resp.Body)
		return
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		// The status has been sent; all the caller can see is a cut-off body.
		p.log.Warn("copying the upstream response", "path", r.URL.Path, "err", err)
	}
}

// fail answers for an upstream call that got no response at all.
func (p *Proxy) fail(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // the caller left first; there is no one to answer
	}
	// The error contains the upstream's address, so it goes to the log, not
	// to the caller.
	p.log.Warn("upstream request failed", "path", r.URL.Path, "err", err)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		writeError(w, http.StatusGatewayTimeout, "upstream_timeout", "the upstream did not answer in time")
		return
	}
	writeError(w, http.StatusBadGateway, "upstream_error", "the upstream could not be reached")
}
