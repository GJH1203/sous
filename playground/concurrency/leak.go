package concurrency

// LeakyFirst runs every source concurrently and returns the first answer.
// It leaks on purpose: ch is unbuffered, so a send blocks until someone
// receives. After the first answer nobody receives again, and every slower
// goroutine stays blocked on its send for the life of the process.
func LeakyFirst(sources []func() int) int {
	ch := make(chan int)
	for _, src := range sources {
		go func() { ch <- src() }()
	}
	return <-ch
}

// First fixes the leak by giving every sender its own slot in the buffer:
// all sends complete whether or not anyone is still listening.
func First(sources []func() int) int {
	ch := make(chan int, len(sources))
	for _, src := range sources {
		go func() { ch <- src() }()
	}
	return <-ch
}
