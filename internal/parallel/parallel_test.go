package parallel

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestEachRunsEveryItemWithinTheBound(t *testing.T) {
	items := make([]int, 50)
	for i := range items {
		items[i] = i
	}
	var running, peak atomic.Int32
	var mu sync.Mutex
	seen := map[int]bool{}
	Each(items, 3, func(i int) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		mu.Lock()
		seen[i] = true
		mu.Unlock()
		running.Add(-1)
	})
	if len(seen) != len(items) {
		t.Errorf("ran %d items, want %d", len(seen), len(items))
	}
	if peak.Load() > 3 {
		t.Errorf("peak concurrency %d, want at most 3", peak.Load())
	}
}
