package concurrency

import (
	"runtime"
	"slices"
	"testing"
)

func TestProduceConsume(t *testing.T) {
	tests := []struct {
		name       string
		n, workers int
	}{
		{"more jobs than workers", 20, 3},
		{"more workers than jobs", 2, 5},
		{"no jobs", 0, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := runtime.NumGoroutine()

			got := ProduceConsume(tt.n, tt.workers, func(x int) int { return x * x })

			var want []int
			for i := 1; i <= tt.n; i++ {
				want = append(want, i*i)
			}
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Errorf("results = %v, want %v", got, want)
			}
			// A clean exit means every goroutine ProduceConsume started is gone.
			waitForGoroutines(t, before)
		})
	}
}
