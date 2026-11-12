// Package concurrency is a warm-up: goroutines, locks, atomics, channels,
// select and context cancellation.
package concurrency

import (
	"sync"
	"sync/atomic"
)

// hammer calls inc perWorker times from each of workers goroutines and
// returns once all of them are done. The three counters differ only in inc.
func hammer(workers, perWorker int, inc func()) {
	var wg sync.WaitGroup
	for range workers {
		// wg.Go counts the goroutine before starting it, so Wait cannot
		// return early because some goroutine had not been scheduled yet.
		wg.Go(func() {
			for range perWorker {
				inc()
			}
		})
	}
	wg.Wait()
}

// RacyCount is wrong on purpose. n++ is load, add, store: two goroutines can
// load the same value, and one of the two increments is lost.
func RacyCount(workers, perWorker int) int {
	n := 0
	hammer(workers, perWorker, func() { n++ })
	return n
}

// MutexCount makes the load-add-store exclusive.
func MutexCount(workers, perWorker int) int {
	var mu sync.Mutex
	n := 0
	hammer(workers, perWorker, func() {
		mu.Lock()
		n++
		mu.Unlock()
	})
	// Reading n without the lock is safe: Wait inside hammer guarantees
	// every goroutine's writes are visible from here on.
	return n
}

// AtomicCount turns the increment into one indivisible hardware operation,
// so no lock is needed. This only works for a single variable.
func AtomicCount(workers, perWorker int) int {
	var n atomic.Int64
	hammer(workers, perWorker, func() { n.Add(1) })
	return int(n.Load())
}
