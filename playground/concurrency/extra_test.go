package concurrency

import (
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
