package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

// timeout names the limit that ran out. It is an error so that it can be a
// context's cause: whatever the context cut short, the gateway can then tell
// which limit did it.
type timeout string

const (
	connectTimeout   timeout = "connect"
	firstByteTimeout timeout = "first_byte"
	idleTimeout      timeout = "idle"
	totalTimeout     timeout = "total"
)

func (t timeout) Error() string { return string(t) + " timeout" }

// code is the error code the caller gets, such as "first_byte_timeout".
func (t timeout) code() string { return string(t) + "_timeout" }

// timeoutOf says which timeout ended a request, if one did. The first-byte,
// idle and total timeouts cancel the request's context; the connect timeout
// comes back from the dial as the error.
func timeoutOf(ctx context.Context, err error) (timeout, bool) {
	var t timeout
	if errors.As(context.Cause(ctx), &t) || errors.As(err, &t) {
		return t, true
	}
	return "", false
}

// deadline is when a request that arrived at arrived must be done: total
// later, or sooner if the caller asks for it with X-Timeout-Ms.
func deadline(r *http.Request, arrived time.Time, total time.Duration) (time.Time, error) {
	limit := total
	if v := r.Header.Get("X-Timeout-Ms"); v != "" {
		ms, err := strconv.ParseInt(v, 10, 64)
		if err != nil || ms <= 0 {
			return time.Time{}, fmt.Errorf("X-Timeout-Ms must be a whole number of milliseconds above 0, got %q", v)
		}
		// Compared in milliseconds: a huge value would overflow as a
		// Duration.
		if ms < total.Milliseconds() {
			limit = time.Duration(ms) * time.Millisecond
		}
	}
	return arrived.Add(limit), nil
}

// dialFunc is the type of http.Transport's DialContext.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// connectKey is the context key for a request's connect timeout. The
// Transport dials with the request's context values, though not with its
// cancellation, so the value gets to dialWithin.
type connectKey struct{}

// dialWithin wraps dial so that it gives up after the connect timeout in the
// context, with connectTimeout as the error.
func dialWithin(dial dialFunc) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		limit, ok := ctx.Value(connectKey{}).(time.Duration)
		if !ok {
			// Every request the proxy sends has one, so this is a bug. Without
			// a limit, a dial could take minutes.
			return nil, errors.New("the request has no connect timeout")
		}
		ctx, cancel := context.WithTimeout(ctx, limit)
		defer cancel() // a connection, once made, outlives its dial's context
		conn, err := dial(ctx, network, addr)
		// The error says whether time ran out, not ctx: the dialer also puts
		// ctx's deadline on the socket, and that often fires a moment before
		// ctx's own timer. This limit is the only one the dial has, so a
		// timeout can only be this one.
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			// Both: the first for timeoutOf, the second, with the address,
			// for the log.
			return nil, fmt.Errorf("%w: %w", connectTimeout, err)
		}
		return conn, err
	}
}
