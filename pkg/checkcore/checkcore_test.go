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

func TestCheckTargetScheme(t *testing.T) {
	for _, tc := range []struct {
		config, target string
		ok             bool
	}{
		{"https://example.com/x.repo", "https://mirror.example.com/el9/", true},
		{"https://example.com/x.repo", "http://mirror.example.com/el9/", true},
		{"https://example.com/x.repo", "file:///etc", false},
		{"https://example.com/x.repo", "/etc", false},
		{"http://example.com/x.list", "s3://victim-bucket/repo", false},
		{"http://example.com/x.list", "sftp://internal-host/srv", false},
		{"/etc/yum.repos.d/x.repo", "file:///srv/mirror", true},
		{"/etc/apt/sources.list", "/srv/mirror", true},
	} {
		if err := CheckTargetScheme(tc.config, tc.target); (err == nil) != tc.ok {
			t.Errorf("CheckTargetScheme(%q, %q) = %v, want ok=%v", tc.config, tc.target, err, tc.ok)
		}
	}
}
