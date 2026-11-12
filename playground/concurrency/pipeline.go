package concurrency

import "sync"

// ProduceConsume has one producer send 1..n to workers consumers, which apply
// process to each number. It returns every result, in no particular order.
func ProduceConsume(n, workers int, process func(int) int) []int {
	jobs := make(chan int)
	results := make(chan int)

	go func() {
		// Closing is the "no more work" signal, replacing a sentinel value:
		// every consumer's range loop ends once jobs is closed and drained.
		// Only the sending side may close.
		defer close(jobs)
		for i := 1; i <= n; i++ {
			jobs <- i
		}
	}()

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for j := range jobs {
				results <- process(j)
			}
		})
	}

	// results may only be closed after the last consumer is done. Calling
	// Wait right here instead would deadlock: consumers would block sending
	// to results while this goroutine is not yet reading it.
	go func() {
		wg.Wait()
		close(results)
	}()

	var out []int
	for r := range results {
		out = append(out, r)
	}

	return out
}
