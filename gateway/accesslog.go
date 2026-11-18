package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// requestInfo is what the access log reports but only the inner layers
// learn. A context value reaches only the layers below the one that sets it,
// never the ones above, so the access log puts an empty requestInfo in the
// context, and the layers below fill it in.
type requestInfo struct {
	caller   string
	model    string // the logical name the caller asked for
	upstream string
	stream   bool
	timeout  string // which timeout ended the request, if one did
	attempts int    // how many times the upstream was tried
}

// infoFrom returns the request's requestInfo. Without the access log there
// is none, and it returns a blank one that nobody reads, so that callers can
// fill it in without checking.
func infoFrom(ctx context.Context) *requestInfo {
	if info, ok := ctx.Value(infoKey).(*requestInfo); ok {
		return info
	}
	return &requestInfo{}
}

// recorder passes everything on to the server's ResponseWriter, and notes
// what the access log needs: the status, and when the first byte was written.
type recorder struct {
	http.ResponseWriter
	start     time.Time
	status    int
	firstByte time.Duration
}

func (rec *recorder) WriteHeader(status int) {
	if rec.status == 0 {
		rec.status = status
	}
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *recorder) Write(p []byte) (int, error) {
	if rec.firstByte == 0 {
		rec.firstByte = time.Since(rec.start)
	}
	return rec.ResponseWriter.Write(p)
}

// Unwrap hands http.ResponseController the server's own ResponseWriter.
// Embedding the interface gives the recorder only the interface's three
// methods; without Unwrap it could not flush.
func (rec *recorder) Unwrap() http.ResponseWriter {
	return rec.ResponseWriter
}

// withAccessLog writes one line for each request once it is done: who asked,
// for which feature, which model and upstream served it, how it ended and how
// long it took. Never a body, and no header but the feature tag.
func withAccessLog(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := &requestInfo{}
		rec := &recorder{ResponseWriter: w, start: time.Now()}
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), infoKey, info)))

		status := rec.status
		if r.Context().Err() != nil {
			status = 499 // nginx's "client closed request": the caller left first
		} else if status == 0 {
			// No WriteHeader: the server sent 200 with the first Write, or
			// will send an empty 200 now.
			status = http.StatusOK
		}
		log.Info("request",
			"request_id", requestIDFrom(r.Context()),
			"caller", info.caller,
			"feature", r.Header.Get("X-Feature"),
			"path", r.URL.Path,
			"model", info.model,
			"upstream", info.upstream,
			"stream", info.stream,
			"status", status,
			"timeout", info.timeout,
			"attempts", info.attempts,
			"first_byte_ms", rec.firstByte.Milliseconds(),
			"total_ms", time.Since(rec.start).Milliseconds(),
		)
	})
}
