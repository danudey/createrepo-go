// Package checkcore is the scaffolding both tools' check command is built on:
// the check levels, the result record, a concurrency-safe collector, and the
// HTTP fetch for a client configuration file named by URL. What each level
// actually checks is format-specific and lives in each format's repocheck.
package checkcore

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Level selects how deep the checks go. Each level is cumulative.
type Level int

// The available check levels, in increasing order of cost.
const (
	LevelMetadata Level = iota // the index and the files it covers, present and valid
	LevelHead                  // + packages exist with the correct size
	LevelFetch                 // + packages downloaded and verified
)

func (l Level) String() string {
	switch l {
	case LevelMetadata:
		return "metadata"
	case LevelHead:
		return "head"
	case LevelFetch:
		return "fetch"
	default:
		return "unknown"
	}
}

// ParseLevel parses a Level from its string name.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "metadata", "meta":
		return LevelMetadata, nil
	case "head", "exists":
		return LevelHead, nil
	case "fetch", "full", "download":
		return LevelFetch, nil
	default:
		return 0, fmt.Errorf("invalid level %q (want metadata|head|fetch)", s)
	}
}

// Status is the outcome of a single check.
type Status string

// The outcomes a single check can report. Only StatusFail means the repository
// is broken; StatusWarn and StatusSkip leave the run successful.
const (
	StatusOK   Status = "OK"
	StatusFail Status = "FAIL"
	StatusWarn Status = "WARN"
	StatusSkip Status = "SKIP"
)

// Result is one validated artifact.
type Result struct {
	Target string // the label of the repository the artifact belongs to
	Kind   string // the index, a metadata file, or a package identifier
	Loc    string // backend-relative path or URL of the artifact
	Status Status
	Detail string
}

// Summarize counts results by status.
func Summarize(results []Result) (ok, fail, warn, skip int) {
	for _, r := range results {
		switch r.Status {
		case StatusOK:
			ok++
		case StatusFail:
			fail++
		case StatusWarn:
			warn++
		case StatusSkip:
			skip++
		}
	}
	return ok, fail, warn, skip
}

// Collector accumulates results from concurrent checks, logging each one as it
// arrives.
type Collector struct {
	mu      sync.Mutex
	results []Result
	logf    func(format string, args ...any)
}

// NewCollector returns a Collector that logs each result through logf, which
// may be nil.
func NewCollector(logf func(string, ...any)) *Collector {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Collector{logf: logf}
}

var symbols = map[Status]string{StatusOK: "  ok ", StatusFail: "FAIL ", StatusWarn: "warn ", StatusSkip: "skip "}

// Add records r. It is safe to call from several goroutines.
func (c *Collector) Add(r Result) {
	c.mu.Lock()
	c.results = append(c.results, r)
	c.mu.Unlock()
	c.logf("[%s] %s: %s %s", symbols[r.Status], r.Target, r.Kind, r.Detail)
}

// Logf writes a progress note through the collector's log.
func (c *Collector) Logf(format string, args ...any) { c.logf(format, args...) }

// Results returns a copy of the accumulated results.
func (c *Collector) Results() []Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Result, len(c.results))
	copy(out, c.results)
	return out
}

// OpCtx derives a per-operation context bounded by timeout; zero means none.
func OpCtx(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// FetchOverHTTP returns a function that downloads a client configuration file
// (a .repo file, an apt sources file) over HTTP(S).
func FetchOverHTTP(ctx context.Context, timeout time.Duration, userAgent string) func(string) ([]byte, error) {
	client := &http.Client{Timeout: timeout}
	return func(rawURL string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP %s", resp.Status)
		}
		return io.ReadAll(resp.Body)
	}
}
