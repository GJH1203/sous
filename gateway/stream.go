package gateway

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"mime"
	"net/http"
	"time"
)

// isEventStream reports whether a response is a stream of server-sent events.
// The response decides, not the request's "stream" flag: an upstream asked to
// stream still answers a bad request with a plain JSON error.
func isEventStream(h http.Header) bool {
	mediaType, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	return err == nil && mediaType == "text/event-stream"
}

// stream relays an event stream one whole event at a time and flushes after
// each, so the caller gets every event as soon as the upstream sends it. If
// the upstream sends nothing for idle, stop cancels ctx, the request's
// context, which ends the stream.
func (p *Proxy) stream(ctx context.Context, w http.ResponseWriter, r *http.Request, body io.Reader, idle time.Duration, stop context.CancelCauseFunc) {
	rc := http.NewResponseController(w)
	// Restarted at every event: a stream may run as long as the deadline
	// allows, but not fall silent for longer than idle.
	timer := time.AfterFunc(idle, func() { stop(idleTimeout) })
	defer timer.Stop()
	events := bufio.NewScanner(body)
	events.Split(splitEvents)
	for events.Scan() {
		timer.Reset(idle)
		if _, err := w.Write(events.Bytes()); err != nil {
			return // the caller is gone; returning cancels the upstream request
		}
		// Without a flush, events wait in the server's write buffer and reach
		// the caller a few kilobytes at a time.
		if err := rc.Flush(); err != nil {
			p.log.Warn("flushing the stream", "request_id", requestIDFrom(r.Context()), "err", err)
			return
		}
	}
	if err := events.Err(); err != nil && r.Context().Err() == nil {
		// The status went out before the first event, so it cannot report
		// this failure any more. An error event is all that is left.
		p.log.Warn("upstream stream broke off", "request_id", requestIDFrom(r.Context()), "err", err)
		code, message := "upstream_error", "the upstream stream ended early"
		if t, ok := timeoutOf(ctx, err); ok {
			infoFrom(r.Context()).timeout = string(t)
			code, message = t.code(), "the upstream took too long: the "+t.Error()+" ran out"
		}
		writeStreamError(w, code, message)
		rc.Flush()
	}
}

// splitEvents is a bufio.SplitFunc that cuts a stream into whole events. An
// event ends with a blank line: "\n\n", or "\r\n\r\n", which the spec also
// allows and Python's sse-starlette sends by default. (A lone "\r" is allowed
// too, but rare enough to leave out.)
func splitEvents(data []byte, atEOF bool) (advance int, token []byte, err error) {
	end := -1
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		end = i + 2
	}
	if i := bytes.Index(data, []byte("\r\n\r\n")); i >= 0 && (end < 0 || i+4 < end) {
		end = i + 4
	}
	if end < 0 {
		// No whole event yet, so ask for more. Whatever is left at the end is
		// a cut-off event, which SSE parsers discard anyway, so it is dropped.
		return 0, nil, nil
	}
	return end, data[:end], nil
}
