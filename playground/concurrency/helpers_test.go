package concurrency

import (
	"runtime"
	"testing"
	"time"
)

// waitForGoroutines fails the test unless the goroutine count settles at want
// within a second. A goroutine that has finished its work still needs a
// moment to be fully gone, so one immediate check would be flaky.
func waitForGoroutines(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		got := runtime.NumGoroutine()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines = %d, want %d", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
