package gateway

import (
	"net/http"
	"strings"
)

// forwardedRequestHeaders is an allowlist. A header the gateway does not know
// about, such as a cookie or the caller's own key, never reaches an upstream.
var forwardedRequestHeaders = []string{"Content-Type", "Accept"}

func copyRequestHeader(dst, src http.Header) {
	for _, name := range forwardedRequestHeaders {
		for _, v := range src.Values(name) {
			dst.Add(name, v)
		}
	}
}

// hopByHop headers describe one connection rather than the message, so each
// hop sets its own. The names are in Go's canonical form: "Te", not "TE".
var hopByHop = []string{
	"Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func copyResponseHeader(dst, src http.Header) {
	skip := make(map[string]bool)
	for _, name := range hopByHop {
		skip[name] = true
	}
	// Connection can name more headers that only apply to this connection.
	for _, v := range src.Values("Connection") {
		for name := range strings.SplitSeq(v, ",") {
			skip[http.CanonicalHeaderKey(strings.TrimSpace(name))] = true
		}
	}
	for name, values := range src {
		if skip[name] {
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}
