package web

import (
	"log/slog"
	"net/http"
	"time"
)

// statusRecorder passes everything through to the real ResponseWriter and
// notes the status code on the way, because ResponseWriter has no method to
// read it back.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(code int) {
	rec.status = code
	rec.ResponseWriter.WriteHeader(code)
}

// Logging wraps next and logs one line per request. The query string is left
// out on purpose: it can carry keys or user data.
func Logging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Starts at 200: a handler that never calls WriteHeader gets 200 from
		// its first Write, and that does not go through WriteHeader above.
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start),
		)
	})
}
