package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/GJH1203/sous/gateway/ratelimit"
)

// sweepEvery is how often callerLimits looks for buckets it can drop.
const sweepEvery = time.Minute

// callerLimits holds a token bucket for each caller that has sent a request
// lately. One lock covers them all: a request holds it only for a map lookup
// and a little arithmetic.
type callerLimits struct {
	mu      sync.Mutex
	rules   map[string]RateLimit // by caller name
	buckets map[string]*ratelimit.TokenBucket
	swept   time.Time
}

func newCallerLimits(callers []Caller) *callerLimits {
	rules := make(map[string]RateLimit)
	for _, c := range callers {
		rules[c.Name] = c.RateLimit
	}
	return &callerLimits{rules: rules, buckets: make(map[string]*ratelimit.TokenBucket)}
}

// allow takes a token from caller's bucket. When there is none, it says how
// long until there is.
func (l *callerLimits) allow(caller string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// A full bucket is the same as a new one, so it can go, and the next
	// request makes a new one. Without this, a caller seen once would keep
	// its bucket for good, and once callers can change at runtime, the map
	// would only grow.
	if now.Sub(l.swept) >= sweepEvery {
		for name, b := range l.buckets {
			if b.Full(now) {
				delete(l.buckets, name) // deleting during range is allowed
			}
		}
		l.swept = now
	}
	b, ok := l.buckets[caller]
	if !ok {
		// withAuth lets in only callers from the same list. If some other
		// name got here, its rule is the zero value, and NewTokenBucket
		// panics: a bug, which withRecovery turns into a 500.
		r := l.rules[caller]
		b = ratelimit.NewTokenBucket(r.Burst, r.PerSecond, now)
		l.buckets[caller] = b
	}
	if b.Allow(now) {
		return true, 0
	}
	return false, b.RetryAfter(now)
}

// withRateLimit refuses a request whose caller has used up its bucket. It
// runs right after withAuth, before the body is read: refusing early is the
// cheapest thing the gateway can do.
func withRateLimit(next http.Handler, limits *callerLimits) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, wait := limits.allow(callerFrom(r.Context()), time.Now())
		if !ok {
			// Whole seconds, rounded up: rounded down, a caller that waited
			// exactly that long would come back too early and be refused
			// again.
			secs := (wait + time.Second - 1) / time.Second
			w.Header().Set("Retry-After", strconv.Itoa(int(secs)))
			writeError(w, http.StatusTooManyRequests, "rate_limited",
				fmt.Sprintf("too many requests; try again in %d s", secs))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// slots limits how many requests an upstream gets at once. It is a channel
// used as a semaphore: sending takes a slot, and blocks while all are taken;
// receiving gives one back.
type slots chan struct{}

// errNoSlot means that no slot came free within the queue timeout.
var errNoSlot = errors.New("no slot came free in time")

// acquire takes a slot, waiting up to timeout for one. A caller that leaves
// while waiting leaves the queue at once, with ctx's error.
func (s slots) acquire(ctx context.Context, timeout time.Duration) error {
	select {
	case s <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(timeout):
		return errNoSlot
	}
}

// release gives back a slot that acquire took.
func (s slots) release() {
	<-s
}
