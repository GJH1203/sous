package concurrency

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReceive(t *testing.T) {
	ready := make(chan int, 1)
	ready <- 7
	closed := make(chan int)
	close(closed)
	silent := make(chan int)

	tests := []struct {
		name     string
		ch       chan int
		want     int
		wantErr  error
		wantWait bool // should Receive sit out the whole timeout?
	}{
		{"value ready", ready, 7, nil, false},
		{"closed", closed, 0, ErrClosed, false},
		{"nothing arrives", silent, 0, ErrTimeout, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			got, err := Receive(tt.ch, 100*time.Millisecond)
			waited := time.Since(start) >= 100*time.Millisecond

			if got != tt.want || !errors.Is(err, tt.wantErr) {
				t.Errorf("Receive() = (%d, %v), want (%d, %v)", got, err, tt.want, tt.wantErr)
			}
			if waited != tt.wantWait {
				t.Errorf("waited the full timeout = %v, want %v", waited, tt.wantWait)
			}
		})
	}
}

func TestSlowSumCompletes(t *testing.T) {
	if got, err := SlowSum(context.Background(), 10, time.Millisecond); got != 55 || err != nil {
		t.Errorf("SlowSum() = (%d, %v), want (55, nil)", got, err)
	}
}

func TestSlowSumStopsAtDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	sum, err := SlowSum(ctx, 1000, 10*time.Millisecond) // ~10s if never stopped
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("took %v to stop, want about 50ms", elapsed)
	}
	if full := 1000 * 1001 / 2; sum >= full {
		t.Errorf("sum = %d, want a partial sum below %d", sum, full)
	}
}

// Cancelling a parent cancels everything derived from it. The worker never
// sees the parent; it only watches its own ctx, and still stops and exits.
func TestCancelReachesDerivedContext(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	child, cancelChild := context.WithTimeout(parent, time.Hour)
	defer cancelChild()

	// Buffered so the worker can always deliver its result and exit, even
	// if this test has already given up waiting for it.
	done := make(chan error, 1)
	go func() {
		_, err := SlowSum(child, 1000, 10*time.Millisecond)
		done <- err
	}()

	time.Sleep(30 * time.Millisecond)
	cancelParent()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker goroutine still running 1s after cancel")
	}
}
