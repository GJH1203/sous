package concurrency

import (
	"context"
	"errors"
	"time"
)

var (
	ErrTimeout = errors.New("timed out")
	ErrClosed  = errors.New("channel closed")
)

// Receive waits for one value from ch, but no longer than d.
func Receive(ch <-chan int, d time.Duration) (int, error) {
	select {
	case v, ok := <-ch:
		if !ok {
			// A closed channel never blocks; it yields the zero value at once,
			// and ok is the only way to tell that apart from a real 0.
			return 0, ErrClosed
		}
		return v, nil
	case <-time.After(d):
		return 0, ErrTimeout
	}
}

// SlowSum adds 1..n, waiting step before each addition, and stops as soon as
// ctx is done. It then returns the partial sum and ctx.Err(), which tells a
// deadline (DeadlineExceeded) apart from an explicit cancel (Canceled).
func SlowSum(ctx context.Context, n int, step time.Duration) (int, error) {
	sum := 0
	for i := 1; i <= n; i++ {
		// Wait in a select rather than time.Sleep, so a cancel interrupts the
		// wait itself instead of being noticed only after it.
		select {
		case <-ctx.Done():
			return sum, ctx.Err()
		case <-time.After(step):
		}
		sum += i
	}
	return sum, nil
}
