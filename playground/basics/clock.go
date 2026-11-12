package basics

import "time"

// Clock is the only way this package reads the time, so tests can swap in a
// FakeClock and never sleep. Neither implementation declares that it
// implements Clock: having a Now() time.Time method is enough.
type Clock interface {
	Now() time.Time
}

// RealClock has no state, only behavior, hence the empty struct.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// FakeClock moves only when a test calls Advance.
type FakeClock struct {
	now time.Time
}

func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start}
}

func (c *FakeClock) Now() time.Time { return c.now }

// Advance needs a pointer receiver: with a value receiver it would move a
// copy, and the clock the test holds would never change.
func (c *FakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// Compile-time checks: if either type stops satisfying Clock, the build
// breaks on this line instead of at some distant call site.
var (
	_ Clock = RealClock{}
	_ Clock = (*FakeClock)(nil)
)

// DaysSince reports whole days from t to now. It takes a Clock rather than
// calling time.Now so a test can decide what "now" is.
func DaysSince(c Clock, t time.Time) int {
	return int(c.Now().Sub(t) / (24 * time.Hour))
}
