package gateway

import (
	"bufio"
	"bytes"
	"io"
	"mime"
	"net/http"
)

// isEventStream reports whether a response is a stream of server-sent events.
// The response decides, not the request's "stream" flag: an upstream asked to
// stream still answers a bad request with a plain JSON error.
func isEventStream(h http.Header) bool {
	mediaType, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	return err == nil && mediaType == "text/event-stream"
}

// stream relays an event stream one whole event at a time and flushes after
// each, so the caller gets every event as soon as the upstream sends it.
func (p *Proxy) stream(w http.ResponseWriter, r *http.Request, body io.Reader) {
	rc := http.NewResponseController(w)
	events := bufio.NewScanner(body)
	events.Split(splitEvents)
	for events.Scan() {
		if _, err := w.Write(events.Bytes()); err != nil {
			return // the caller is gone; returning cancels the upstream request
		}
		// Without a flush, events wait in the server's write buffer and reach
		// the caller a few kilobytes at a time.
		if err := rc.Flush(); err != nil {
			p.log.Warn("flushing the stream", "path", r.URL.Path, "err", err)
			return
		}
	}
	if err := events.Err(); err != nil && r.Context().Err() == nil {
		// The status went out before the first event, so it cannot report
		// this failure any more. An error event is all that is left.
		p.log.Warn("upstream stream broke off", "path", r.URL.Path, "err", err)
		writeStreamError(w, "upstream_error", "the upstream stream ended early")
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
