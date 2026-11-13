// Package web is a warm-up for net/http: handlers, a middleware, a client
// with a timeout, and both ways of testing them.
package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxEchoBytes caps how much of a request body /echo reads, so one huge
// request cannot use up the server's memory.
const maxEchoBytes = 1 << 20

// Routes returns the playground server. /slow answers only after slowFor,
// which lets a test make the server slower than a client's timeout.
func Routes(slowFor time.Duration) http.Handler {
	mux := http.NewServeMux()
	// A method in the pattern makes the mux answer other methods on the
	// same path with 405, without any code of ours.
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("POST /echo", echo)
	mux.HandleFunc("GET /slow", slow(slowFor))
	return mux
}

func healthz(w http.ResponseWriter, r *http.Request) {
	// No WriteHeader call: the first Write sends 200 on its own.
	fmt.Fprintln(w, "ok")
}

// echo sends the request body back unchanged if it is valid JSON. The bytes
// pass through as they came: decoding and re-encoding could reorder keys or
// rewrite numbers, and the reply would no longer be the same JSON.
func echo(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEchoBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "cannot read request body", http.StatusBadRequest)
		return
	}
	if !json.Valid(body) {
		http.Error(w, "request body is not valid JSON", http.StatusBadRequest)
		return
	}
	// Headers must be set before the first Write. By then they have been
	// sent, and later changes are silently ignored.
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// slow answers after d, unless the client gives up first. r.Context() is
// cancelled when the client goes away, and then nobody is left to answer.
func slow(d time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(d):
			fmt.Fprintln(w, "finally")
		case <-r.Context().Done():
		}
	}
}
