// Package ratelimit decides whether something may happen now, given how
// often it has happened before. It knows nothing of HTTP or goroutines, and
// takes the time as an argument, so its tests need no clock.
package ratelimit

import (
	"fmt"
	"math"
	"time"
)

// TokenBucket lets through bursts of up to its capacity at once, and its
// rate per second on average. Each Allow takes a token, and tokens come back
// at the rate, up to the capacity.
//
// Nothing runs in the background to put tokens back. Each call first works
// out how many the time since the previous call has added.
//
// A TokenBucket is not safe for concurrent use: whoever shares one holds a
// lock around it.
type TokenBucket struct {
	// The bucket holds time rather than tokens: a token is per, the time it
	// takes to come back. Durations are whole nanoseconds, so adding a
	// millisecond a million times makes exactly 1000 s. Adding a float
	// fraction of a token a million times would not, and the error would
	// pile up.
	per   time.Duration // the time one token takes to come back
	full  time.Duration // capacity × per
	level time.Duration // how full the bucket is, as of last
	last  time.Time     // the latest time seen
}

// NewTokenBucket returns a full bucket, so that a new caller can start with
// a burst. capacity must be at least 1, and perSecond above 0 and at most a
// billion. The config check rules anything else out, so another value is a
// bug, and panics.
func NewTokenBucket(capacity int, perSecond float64, now time.Time) *TokenBucket {
	per := float64(time.Second) / perSecond // nanoseconds per token
	// A token takes at least a nanosecond, and a full bucket fits in a
	// Duration. Written as one positive condition so that NaN fails it too:
	// every comparison with NaN is false.
	if !(capacity >= 1 && per >= 1 && per*float64(capacity) < math.MaxInt64) {
		panic(fmt.Sprintf("ratelimit: no bucket holds %d tokens at %v a second", capacity, perSecond))
	}
	full := time.Duration(capacity) * time.Duration(per)
	return &TokenBucket{per: time.Duration(per), full: full, level: full, last: now}
}

// refill adds the time that has passed between the latest time seen and
// now.
func (b *TokenBucket) refill(now time.Time) {
	// now can be earlier than a time already seen: two requests read the
	// clock, and the later one gets the lock first. Time running backwards
	// adds nothing, and last stays the latest time seen, so that the same
	// stretch of time is not counted twice when now catches up.
	if !now.After(b.last) {
		return
	}
	// Never more than the room left: after a long idle, level + elapsed
	// could overflow and turn negative.
	b.level += min(now.Sub(b.last), b.full-b.level)
	b.last = now
}

// Allow takes a token and reports true if there is one at now.
func (b *TokenBucket) Allow(now time.Time) bool {
	b.refill(now)
	if b.level < b.per {
		return false
	}
	b.level -= b.per
	return true
}

// RetryAfter says how long after now the next token will be there: 0 if
// there is one already.
func (b *TokenBucket) RetryAfter(now time.Time) time.Duration {
	b.refill(now)
	if b.level >= b.per {
		return 0
	}
	// Counted from last, not now: they differ when the clock has gone back.
	return b.last.Add(b.per - b.level).Sub(now)
}
