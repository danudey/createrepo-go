// Package parallel runs a function over a slice with bounded concurrency.
package parallel

import "sync"

// Each calls fn for every item, at most conc at a time (conc < 1 means one at
// a time), and returns once every call has. fn may run on several goroutines
// at once, so whatever it writes to must be safe for that.
func Each[T any](items []T, conc int, fn func(T)) {
	sem := make(chan struct{}, max(conc, 1))
	var wg sync.WaitGroup
	for _, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn(it)
		}()
	}
	wg.Wait()
}
