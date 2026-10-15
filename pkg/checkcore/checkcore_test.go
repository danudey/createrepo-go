package checkcore

import (
	"fmt"
	"sync"
	"testing"
)

func TestParseLevel(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Level
	}{{"meta", LevelMetadata}, {" HEAD ", LevelHead}, {"download", LevelFetch}} {
		if got, err := ParseLevel(tc.in); err != nil || got != tc.want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	if _, err := ParseLevel("deep"); err == nil {
		t.Error(`ParseLevel("deep") succeeded`)
	}
}

func TestCollectorIsSafeAndSummarizes(t *testing.T) {
	var logged int
	var mu sync.Mutex
	c := NewCollector(func(string, ...any) { mu.Lock(); logged++; mu.Unlock() })
	var wg sync.WaitGroup
	statuses := []Status{StatusOK, StatusFail, StatusWarn, StatusSkip}
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Add(Result{Kind: fmt.Sprint(i), Status: statuses[i%4]})
		}()
	}
	wg.Wait()
	ok, fail, warn, skip := Summarize(c.Results())
	if ok != 10 || fail != 10 || warn != 10 || skip != 10 || logged != 40 {
		t.Errorf("Summarize = %d/%d/%d/%d, logged %d; want 10 each and 40 logged", ok, fail, warn, skip, logged)
	}
}
