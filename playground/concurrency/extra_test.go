package concurrency

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// Twenty 50ms jobs take a full second one after another; four consumers
// working in parallel should need about a quarter of that.
func TestProduceConsumeRunsInParallel(t *testing.T) {
	start := time.Now()
	ProduceConsume(20, 4, func(x int) int {
		time.Sleep(50 * time.Millisecond)
		return x
	})
	if elapsed := time.Since(start); elapsed > 600*time.Millisecond {
		t.Errorf("took %v, want well under the 1s a single consumer needs", elapsed)
	}
}

func TestCountersWithNoWorkers(t *testing.T) {
	for name, count := range map[string]func(int, int) int{
		"racy":   RacyCount,
		"mutex":  MutexCount,
		"atomic": AtomicCount,
	} {
		if got := count(0, perWorker); got != 0 {
			t.Errorf("%s with 0 workers = %d, want 0", name, got)
		}
	}
}

// TestFirst lists the fastest source first, so it cannot tell "fastest" from
// "first in the list". Here the fastest is last, and an answer that waited
// for every source would take the slow one's 300ms.
func TestFirstTakesFastestNotFirstListed(t *testing.T) {
	before := runtime.NumGoroutine()
	start := time.Now()

	got := First([]func() int{after(300*time.Millisecond, 1), after(0, 2)})

	if elapsed := time.Since(start); got != 2 || elapsed > 150*time.Millisecond {
		t.Errorf("First() = %d after %v, want 2 well before 300ms", got, elapsed)
	}
	// Let the slow source finish, or later tests would count its goroutine.
	waitForGoroutines(t, before)
}

// A real 0 and a closed channel both hand back 0; only ok tells them apart.
func TestReceiveRealZero(t *testing.T) {
	ch := make(chan int, 1)
	ch <- 0
	if got, err := Receive(ch, 100*time.Millisecond); got != 0 || err != nil {
		t.Errorf("Receive() = (%d, %v), want (0, nil)", got, err)
	}
}

// A cancel has to cut the current wait short. Waiting with time.Sleep(step)
// and checking ctx afterwards would only stop after the full 2s step.
func TestSlowSumInterruptsLongStep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	sum, err := SlowSum(ctx, 3, 2*time.Second)

	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("took %v to stop, want about 50ms", elapsed)
	}
	if sum != 0 || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("SlowSum() = (%d, %v), want (0, context.DeadlineExceeded)", sum, err)
	}
}
