package ratelimit

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// at is t0 plus d.
func at(d time.Duration) time.Time { return t0.Add(d) }

// allowed calls Allow n times at now and counts the trues.
func allowed(b *TokenBucket, now time.Time, n int) int {
	got := 0
	for range n {
		if b.Allow(now) {
			got++
		}
	}
	return got
}

// A burst gets the capacity at once, then has to wait for the rate.
func TestBurst(t *testing.T) {
	b := NewTokenBucket(5, 2, t0)
	if got := allowed(b, t0, 10); got != 5 {
		t.Fatalf("%d of 10 let through at once, want 5", got)
	}
	if d := b.RetryAfter(t0); d != 500*time.Millisecond {
		t.Errorf("RetryAfter = %v, want 500ms", d)
	}
	if b.Allow(at(500*time.Millisecond - time.Nanosecond)) {
		t.Error("a token came back early")
	}
	if !b.Allow(at(500 * time.Millisecond)) {
		t.Error("no token after 500ms")
	}
}

// Requests at the rate all get through; requests at twice the rate get
// half through.
func TestSteadyRate(t *testing.T) {
	b := NewTokenBucket(3, 10, t0)
	allowed(b, t0, 3) // spend the burst
	for i := 1; i <= 100; i++ {
		if !b.Allow(at(time.Duration(i) * 100 * time.Millisecond)) {
			t.Fatalf("request %d, at the rate, was refused", i)
		}
	}
	got := 0
	for i := 1; i <= 100; i++ {
		if b.Allow(at(10*time.Second + time.Duration(i)*50*time.Millisecond)) {
			got++
		}
	}
	if got != 50 {
		t.Errorf("%d of 100 at twice the rate let through, want 50", got)
	}
}

// A day of not being used fills the bucket, and no more.
func TestIdleDoesNotOverfill(t *testing.T) {
	b := NewTokenBucket(5, 2, t0)
	allowed(b, t0, 5)
	if got := allowed(b, at(24*time.Hour), 100); got != 5 {
		t.Errorf("%d let through after a day, want the capacity, 5", got)
	}
}

// A time earlier than one already seen neither adds tokens nor takes them
// away, and the stretch it covers again is not counted twice.
func TestClockGoesBack(t *testing.T) {
	b := NewTokenBucket(2, 1, t0)
	allowed(b, at(10*time.Second), 2) // empty at 10 s
	if b.Allow(at(5 * time.Second)) {
		t.Error("a token at 5 s, after the bucket was emptied at 10 s")
	}
	if b.level < 0 {
		t.Errorf("the bucket went negative: %v", b.level)
	}
	// The next token comes at 11 s, which is 6 s after 5 s.
	if d := b.RetryAfter(at(5 * time.Second)); d != 6*time.Second {
		t.Errorf("RetryAfter at 5 s = %v, want 6s", d)
	}
	// From 10 s to 11 s is one second: one token, not one for each of the
	// seconds from 5 s.
	if got := allowed(b, at(11*time.Second), 2); got != 1 {
		t.Errorf("%d let through at 11 s, want 1", got)
	}
}

// A million calls, one a millisecond, at half a token a second. Each call
// adds a two-thousandth of a token; rounding must not add up to a lost or
// extra token, or a token that comes late, over 1000 s. (A bucket that
// kept a float count of tokens failed this: token 2 came at 4.002 s.)
func TestNoDrift(t *testing.T) {
	b := NewTokenBucket(1, 0.5, t0)
	var times []time.Duration
	for i := range 1_000_000 {
		d := time.Duration(i) * time.Millisecond
		if b.Allow(at(d)) {
			times = append(times, d)
		}
	}
	if len(times) != 500 {
		t.Fatalf("%d let through, want 500", len(times))
	}
	for k, d := range times {
		if want := time.Duration(k) * 2 * time.Second; d != want {
			t.Fatalf("token %d at %v, want %v", k, d, want)
		}
	}
}

// Waiting exactly RetryAfter always finds a token, and waiting a nanosecond
// less never does, whatever the rate and however full the bucket was. (A
// float count of tokens failed this: after the wait it held
// 0.9999999999999999 tokens.)
func TestRetryAfterIsExact(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 10_000 {
		rate := []float64{0.3, 1, 3, 7, 1.0 / 7, 10, 1000, 0.01}[r.IntN(8)]
		b := NewTokenBucket(1+r.IntN(3), rate, t0)
		now := t0
		for range 1 + r.IntN(20) {
			now = now.Add(time.Duration(r.Int64N(int64(2 * time.Second / time.Duration(math.Ceil(rate))))))
			b.Allow(now)
		}
		wait := b.RetryAfter(now)
		if wait < 0 {
			t.Fatalf("rate %v: RetryAfter = %v", rate, wait)
		}
		if wait > 0 {
			early := *b // a copy, so the bucket itself is not touched
			if early.Allow(now.Add(wait - 1)) {
				t.Fatalf("rate %v: a token a nanosecond before RetryAfter = %v", rate, wait)
			}
		}
		if !b.Allow(now.Add(wait)) {
			t.Fatalf("rate %v: no token after waiting RetryAfter = %v", rate, wait)
		}
	}
}

// A bucket made at the zero time and first used today has been idle for
// 2026 years, far more than a Duration holds. It is full, not overflowed.
func TestLongIdleDoesNotOverflow(t *testing.T) {
	b := NewTokenBucket(5, 1, time.Time{})
	if got := allowed(b, t0, 10); got != 5 {
		t.Errorf("%d let through, want 5", got)
	}
}

func TestRetryAfterIsZeroWithATokenLeft(t *testing.T) {
	b := NewTokenBucket(3, 1, t0)
	b.Allow(t0)
	if d := b.RetryAfter(t0); d != 0 {
		t.Errorf("RetryAfter = %v with two tokens left", d)
	}
	// Empty it; half a second later a token is half back: 0.5 s to go.
	allowed(b, t0, 2)
	if d := b.RetryAfter(at(500 * time.Millisecond)); d != 500*time.Millisecond {
		t.Errorf("RetryAfter = %v, want 500ms", d)
	}
}

func TestBadSettingsPanic(t *testing.T) {
	for _, tt := range []struct {
		capacity int
		rate     float64
	}{
		{0, 1}, {-1, 1}, {1, 0}, {1, -2}, {1, math.NaN()},
		{1, 3e9},         // a token every third of a nanosecond
		{1 << 40, 1e-12}, // a full bucket longer than a Duration
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("capacity %d, rate %v: no panic", tt.capacity, tt.rate)
				}
			}()
			NewTokenBucket(tt.capacity, tt.rate, t0)
		}()
	}
}
