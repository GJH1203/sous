package concurrency

import (
	"runtime"
	"testing"
	"time"
)

// after returns a source that answers v once d has passed.
func after(d time.Duration, v int) func() int {
	return func() int {
		time.Sleep(d)
		return v
	}
}

func TestFirst(t *testing.T) {
	tests := []struct {
		name       string
		first      func([]func() int) int
		wantLeaked int
	}{
		{"unbuffered leaks the losers", LeakyFirst, 2},
		{"buffered leaks nothing", First, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := runtime.NumGoroutine()
			sources := []func() int{
				after(0, 1),
				after(20*time.Millisecond, 2),
				after(30*time.Millisecond, 3),
			}

			if got := tt.first(sources); got != 1 {
				t.Errorf("first answer = %d, want 1", got)
			}

			// Give the slow sources time to answer. Whether their goroutines
			// can then exit is the only difference between the two versions.
			time.Sleep(100 * time.Millisecond)
			waitForGoroutines(t, before+tt.wantLeaked)
		})
	}
}
