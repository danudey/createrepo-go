package progress

import (
	"strings"
	"testing"
	"time"
)

func TestBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{3 * 1024 * 1024 * 1024, "3.0 GiB"},
	}
	for _, c := range cases {
		if got := Bytes(c.n); got != c.want {
			t.Errorf("Bytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{-time.Second, "0s"},
		{5 * time.Second, "5s"},
		{94 * time.Second, "1m34s"},
		{time.Hour + 3*time.Minute, "1h03m"},
	}
	for _, c := range cases {
		if got := Duration(c.d); got != c.want {
			t.Errorf("Duration(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestBar(t *testing.T) {
	cases := []struct {
		frac float64
		want string
	}{
		{0, "[----------]"},
		{0.5, "[=====-----]"},
		{1, "[==========]"},
		// A total that turns out to be wrong must not draw outside the bar.
		{1.5, "[==========]"},
		{-1, "[----------]"},
	}
	for _, c := range cases {
		if got := bar(c.frac, 10); got != c.want {
			t.Errorf("bar(%v, 10) = %q, want %q", c.frac, got, c.want)
		}
	}
}

func TestFractionHandlesAnUnknownTotal(t *testing.T) {
	if got := fraction(10, 0); got != 0 {
		t.Errorf("fraction(10, 0) = %v, want 0", got)
	}
}

func TestClip(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly-10", 10, "exactly-10"},
		{"a-long-package-name", 10, "a-long-..."},
		{"anything", 2, "an"},
		{"anything", 0, ""},
	}
	for _, c := range cases {
		if got := clip(c.s, c.n); got != c.want {
			t.Errorf("clip(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
	}
}

func TestPadHoldsTheColumnWidth(t *testing.T) {
	for _, name := range []string{"a", "exactly-10", "a-package-name-far-too-long-for-this-column.rpm"} {
		if got := pad(name, 10); len(got) != 10 {
			t.Errorf("pad(%q, 10) = %q (%d columns), want exactly 10", name, got, len(got))
		}
	}
}

// The bar's column is what makes the display readable while it updates: it must
// depend on the terminal's width alone, not on the figures printed beside it.
func TestTheBarKeepsItsColumnAsTheFiguresChange(t *testing.T) {
	tr, _ := newTestTracker(t)
	tr.tty = true
	tr.Begin("copy", 2, 4_000_000)
	it := tr.Item("some-package-1.2.3-1.el9.x86_64.rpm", 2_000_000)

	var column int
	for _, moved := range []int64{1, 900, 90_000, 1_500_000} {
		it.Add(moved)
		tr.mu.Lock()
		line := tr.itemLine(it, 0, 80)
		tr.mu.Unlock()
		at := strings.Index(line, "[")
		if at < 0 {
			t.Fatalf("no bar in the transfer line:\n%s", line)
		}
		if column == 0 {
			column = at
			continue
		}
		if at != column {
			t.Errorf("the bar moved from column %d to %d after %s were transferred:\n%s",
				column, at, Bytes(moved), line)
		}
	}
}

func TestRateMeterFollowsTheCurrentSpeed(t *testing.T) {
	var m rateMeter
	start := time.Now()
	m.reset(start)
	if got := m.perSecond(); got != 0 {
		t.Errorf("a meter with no samples reports %v, want 0", got)
	}

	// A steady megabyte a second settles on a megabyte a second.
	total := int64(0)
	for i := 1; i <= 20; i++ {
		total += 1 << 20
		m.observe(start.Add(time.Duration(i)*time.Second), total)
	}
	got := m.perSecond()
	if got < 0.9*(1<<20) || got > 1.1*(1<<20) {
		t.Errorf("rate = %v B/s, want about %v", got, float64(int64(1)<<20))
	}
}

func TestRateMeterToleratesARewind(t *testing.T) {
	var m rateMeter
	start := time.Now()
	m.reset(start)
	m.observe(start.Add(time.Second), 1000)
	// A wrapped reader seeked back to the start un-counts its bytes, so the
	// running total can fall. That is an idle interval, not a negative rate.
	m.observe(start.Add(2*time.Second), 0)
	if got := m.perSecond(); got < 0 {
		t.Errorf("rate after a rewind = %v, want a non-negative rate", got)
	}
}
