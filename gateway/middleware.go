package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
)

// ctxKey is a type of its own, so that no other package's context values
// can collide with the gateway's.
type ctxKey int

const (
	requestIDKey ctxKey = iota
	callerKey
	infoKey
)

// requestIDFrom returns the ID that withRequestID gave the request, or "".
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// callerFrom returns the name of the caller that withAuth let in, or "".
func callerFrom(ctx context.Context) string {
	name, _ := ctx.Value(callerKey).(string)
	return name
}

// withRequestID gives each request an ID, which goes back to the caller and
// on to the upstream, so that one request can be followed through every log.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := rand.Text()
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// withRecovery turns a panic into a 500. The server would survive the panic
// anyway, but it would close the connection without an answer, and log the
// stack as plain text without the request ID.
func withRecovery(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v) // a deliberate abort: the server cuts the connection
			}
			log.Error("panic", "request_id", requestIDFrom(r.Context()), "path", r.URL.Path,
				"panic", v, "stack", string(debug.Stack()))
			// Once the response has started, its status cannot change any
			// more, and the caller sees this error appended to it.
			writeError(w, http.StatusInternalServerError, "internal_error", "the gateway failed while handling the request")
		}()
		next.ServeHTTP(w, r)
	})
}

// withAuth lets a request in only with a known caller's key, and puts the
// caller's name in its context.
func withAuth(next http.Handler, callers []Caller) http.Handler {
	type known struct {
		name string
		hash [sha256.Size]byte
	}
	// Keys are compared by their hashes, which all have the same length.
	// ConstantTimeCompare returns at once for slices of different lengths,
	// so comparing the keys themselves would give away a key's length.
	keys := make([]known, len(callers))
	for i, c := range callers {
		keys[i] = known{c.Name, sha256.Sum256([]byte(c.APIKey))}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server trims the value, so "Bearer " with no key arrives as
		// "Bearer" and has no prefix to cut.
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			unauthorized(w, "missing_api_key", "the request needs an Authorization: Bearer header")
			return
		}
		got := sha256.Sum256([]byte(key))
		caller := ""
		for _, k := range keys {
			if subtle.ConstantTimeCompare(got[:], k.hash[:]) == 1 {
				caller = k.name
				break
			}
		}
		if caller == "" {
			unauthorized(w, "invalid_api_key", "the API key is not valid")
			return
		}
		infoFrom(r.Context()).caller = caller
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey, caller)))
	})
}

func unauthorized(w http.ResponseWriter, code, message string) {
	// HTTP requires a 401 to name the scheme the client should use.
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, code, message)
}
