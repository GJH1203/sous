package gateway

import (
	"log/slog"
	"net/http"
)

// NewHandler returns everything the gateway serves: proxy on the API paths,
// OpenAI-shaped errors for anything else, all behind the middleware. proxy is
// a parameter so that tests can put any handler there.
func NewHandler(cfg *Config, proxy http.Handler, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	for _, path := range []string{"/v1/chat/completions", "/v1/embeddings"} {
		mux.Handle("POST "+path, proxy)
		// A pattern without a method catches every other method: the one
		// with POST is more specific, so POST still goes to the proxy.
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", "POST")
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", r.Method+" is not allowed here; use POST")
		})
	}
	// Everything no other pattern matches. Without these two, ServeMux would
	// answer in plain text.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "there is nothing at "+r.URL.Path)
	})

	// Each wrap goes around everything before it, so the last one runs
	// first: request ID, access log, recovery, authentication, then the routes.
	var h http.Handler = mux
	h = withAuth(h, cfg.Callers)
	h = withRecovery(h, log)
	h = withAccessLog(h, log)
	h = withRequestID(h)
	return h
}
