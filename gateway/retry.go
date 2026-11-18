package gateway

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// maxAttempts counts the first attempt too.
	maxAttempts = 3
	// The longest wait before the first retry, doubling for each one after,
	// up to maxBackoff.
	firstBackoff = 200 * time.Millisecond
	maxBackoff   = 2 * time.Second
)

// retryable says whether an upstream's answer with this status is a failure
// that another attempt might not repeat. Other 4xx mean the request is wrong,
// and a 500 usually comes from something in this particular request: the same
// input tends to fail the same way.
func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// backoff is how long to wait before retry n, the first being 1: a random
// time up to a ceiling that doubles with each retry. random is in [0, 1).
// Random, so that requests that failed together do not all come back
// together.
func backoff(n int, random float64) time.Duration {
	// Doubled in a loop rather than shifted, which for a large n would
	// overflow.
	ceiling := firstBackoff
	for i := 1; i < n && ceiling < maxBackoff; i++ {
		ceiling *= 2
	}
	return time.Duration(random * float64(min(ceiling, maxBackoff)))
}

// retryAfter is how long an upstream's Retry-After header asks the gateway
// to wait, at now: 0 when there is none, or it makes no sense.
func retryAfter(h http.Header, now time.Time) time.Duration {
	v := h.Get("Retry-After")
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		// Capped, so that a huge value cannot overflow. A day is far past
		// any request's deadline anyway.
		return time.Duration(min(s, 24*60*60)) * time.Second
	}
	// The header's other form is a date.
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// budgetWindow is how many seconds of attempts a retry budget looks back on.
const budgetWindow = 10

// retryBudget caps the retries sent to one upstream: over the last 10 s, a
// fifth of the first attempts, plus 3. Without a cap, when an upstream fails,
// every request in flight retries it at once, and a server that has just come
// back gets its normal load and a wave of retries together. The 3 is for
// light traffic: the app sends a few requests a minute, and a fifth of that
// would allow no retries at all.
//
// The zero value is an empty budget, ready to use. It is safe for concurrent
// use.
type retryBudget struct {
	mu sync.Mutex
	// One count per second of the window, each reused when its second comes
	// round again. Whole seconds make the window between 9 and 10 s long,
	// which is close enough.
	counts [budgetWindow]secondCount
}

type secondCount struct {
	sec             int64 // the Unix second counted
	firsts, retries int
}

// at returns the count for now's second, cleared if it still holds an old
// one.
func (b *retryBudget) at(now time.Time) *secondCount {
	sec := now.Unix()
	c := &b.counts[sec%budgetWindow]
	// Only a newer second clears it. A request that read the clock just
	// before another took the lock may come a second late; it counts there.
	if c.sec < sec {
		*c = secondCount{sec: sec}
	}
	return c
}

// first records a first attempt at now.
func (b *retryBudget) first(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.at(now).firsts++
}

// spend records a retry at now, if the budget has room for one, and says
// whether it had.
func (b *retryBudget) spend(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	var firsts, retries int
	for _, c := range b.counts {
		if now.Unix()-c.sec < budgetWindow {
			firsts += c.firsts
			retries += c.retries
		}
	}
	// retries+1 <= firsts/5 + 3, in whole numbers.
	if 5*(retries+1) > firsts+5*3 {
		return false
	}
	b.at(now).retries++
	return true
}
