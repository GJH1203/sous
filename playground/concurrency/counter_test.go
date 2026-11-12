package concurrency

import (
	"os"
	"testing"
)

const (
	workers   = 4
	perWorker = 100_000
)

func TestSafeCounters(t *testing.T) {
	tests := []struct {
		name  string
		count func(workers, perWorker int) int
	}{
		{"mutex", MutexCount},
		{"atomic", AtomicCount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, want := tt.count(workers, perWorker), workers*perWorker; got != want {
				t.Errorf("count = %d, want %d", got, want)
			}
		})
	}
}

// Skipped unless asked for: under -race, which make check uses, the race
// detector fails this test, and showing that failure is the whole point.
//
//	RACE_DEMO=1 go test -race -run TestRacyCount -v ./playground/concurrency/
func TestRacyCount(t *testing.T) {
	if os.Getenv("RACE_DEMO") == "" {
		t.Skip("set RACE_DEMO=1 to run the data race demo")
	}
	got, want := RacyCount(workers, perWorker), workers*perWorker
	t.Logf("racy count = %d, want %d: lost %d increments", got, want, want-got)
}
