package progress

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// newTestTracker returns a tracker writing to a buffer, in the non-terminal
// mode (no control sequences), with a fixed width so rendering is comparable.
func newTestTracker(t *testing.T) (*Tracker, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	tr := New(buf, true)
	if tr == nil {
		t.Fatal("New returned nil for an enabled tracker")
	}
	tr.width = func() int { return 80 }
	// Long enough that no periodic line lands in the middle of a test.
	tr.plainEvery = time.Hour
	return tr, buf
}

func TestNilTrackerIsInert(t *testing.T) {
	var tr *Tracker
	tr.Begin("upload", 3, 300)
	tr.Skip(1, 100)
	it := tr.Item("one.rpm", 100)
	it.Phase("put")
	it.Add(50)
	it.Done()
	tr.End()

	// A nil item must still hand back the reader it was given.
	r := strings.NewReader("hello")
	if got := it.Reader(r); got != io.Reader(r) {
		t.Errorf("nil Item.Reader wrapped the reader; want it passed through")
	}
	w := io.Discard
	if tr.Wrap(w) != w {
		t.Errorf("nil Tracker.Wrap wrapped the writer; want it passed through")
	}
}

func TestNewDisabledReturnsNil(t *testing.T) {
	if tr := New(&bytes.Buffer{}, false); tr != nil {
		t.Errorf("New(w, false) = %v, want nil", tr)
	}
}

func TestItemProgressCountsTowardsTheOperation(t *testing.T) {
	tr, _ := newTestTracker(t)
	tr.Begin("upload", 2, 300)

	first := tr.Item("one.rpm", 100)
	first.Add(60)
	if got := tr.transferred(); got != 60 {
		t.Errorf("after 60 bytes, operation total = %d, want 60", got)
	}
	first.Add(40)
	first.Done()

	second := tr.Item("two.rpm", 200)
	second.Add(50)
	if got, want := tr.transferred(), int64(150); got != want {
		t.Errorf("operation total = %d, want %d", got, want)
	}
	if got, want := tr.completed(), 1; got != want {
		t.Errorf("completed items = %d, want %d", got, want)
	}
	second.Done()

	// Done is idempotent: a deferred Done after an explicit one must not count
	// the item twice.
	second.Done()
	if got, want := tr.completed(), 2; got != want {
		t.Errorf("completed items after a repeated Done = %d, want %d", got, want)
	}
}

func TestSkipTakesWorkOffTheTotals(t *testing.T) {
	tr, _ := newTestTracker(t)
	tr.Begin("copy", 3, 300)
	tr.Skip(1, 100)

	tr.mu.Lock()
	items, bytes := tr.totalItems, tr.totalBytes
	tr.mu.Unlock()
	if items != 2 || bytes != 200 {
		t.Errorf("after skipping one item of 100 bytes: %d items / %d bytes, want 2 / 200", items, bytes)
	}

	// A skip may never take the totals below what is already done.
	it := tr.Item("one.rpm", 200)
	it.Add(200)
	it.Done()
	tr.Skip(5, 5000)
	tr.mu.Lock()
	items, bytes = tr.totalItems, tr.totalBytes
	tr.mu.Unlock()
	if items != 1 || bytes != 200 {
		t.Errorf("over-skipping gave %d items / %d bytes, want 1 / 200 (the work already done)", items, bytes)
	}
}

func TestItemFromAnEndedOperationIsIgnored(t *testing.T) {
	tr, _ := newTestTracker(t)
	tr.Begin("upload", 1, 100)
	it := tr.Item("one.rpm", 100)
	tr.End()

	tr.Begin("upload", 1, 100)
	it.Add(100) // the stale handle must not pollute the new operation
	it.Done()
	if got := tr.transferred(); got != 0 {
		t.Errorf("a stale item added %d bytes to the new operation, want 0", got)
	}
	if got := tr.completed(); got != 0 {
		t.Errorf("a stale item completed %d items of the new operation, want 0", got)
	}
}

func TestItemOutsideAnOperationIsNil(t *testing.T) {
	tr, _ := newTestTracker(t)
	if it := tr.Item("one.rpm", 100); it != nil {
		t.Errorf("Item outside Begin/End = %v, want nil", it)
	}
}

func TestReaderCountsBytesRead(t *testing.T) {
	tr, _ := newTestTracker(t)
	tr.Begin("upload", 1, 12)
	it := tr.Item("one.rpm", 12)

	r := it.Reader(strings.NewReader("hello, world"))
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if got := tr.transferred(); got != 12 {
		t.Errorf("counted %d bytes, want 12", got)
	}
}

func TestReaderKeepsSeekableBodiesSeekable(t *testing.T) {
	tr, _ := newTestTracker(t)
	tr.Begin("upload", 1, 12)
	it := tr.Item("one.rpm", 12)

	// The S3 and GCS backends need a seekable body; losing that would make them
	// spill the upload to a temporary file.
	wrapped := it.Reader(strings.NewReader("hello, world"))
	rs, ok := wrapped.(io.ReadSeeker)
	if !ok {
		t.Fatal("wrapping a ReadSeeker produced a plain Reader")
	}

	// A hashing pass followed by a rewind must not count the body twice.
	if _, err := io.Copy(io.Discard, rs); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	if got := tr.transferred(); got != 0 {
		t.Errorf("after rewinding, counted %d bytes, want 0", got)
	}
	if _, err := io.Copy(io.Discard, rs); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := tr.transferred(); got != 12 {
		t.Errorf("after the real pass, counted %d bytes, want 12", got)
	}
}

func TestEndSummarizesWhatMoved(t *testing.T) {
	tr, buf := newTestTracker(t)
	tr.Begin("upload", 2, 300)
	it := tr.Item("one.rpm", 100)
	it.Add(100)
	it.Done()
	tr.End()

	got := buf.String()
	for _, want := range []string{"upload", "1/2", "100 B", "300 B"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q does not mention %q", strings.TrimSpace(got), want)
		}
	}
}

func TestEndSaysNothingWhenNothingMoved(t *testing.T) {
	tr, buf := newTestTracker(t)
	tr.Begin("upload", 2, 300)
	tr.End()
	if got := buf.String(); got != "" {
		t.Errorf("an operation that moved nothing printed %q, want nothing", got)
	}
}

func TestEmptyOperationIsNotStarted(t *testing.T) {
	tr, buf := newTestTracker(t)
	tr.Begin("upload", 0, 0)
	if it := tr.Item("one.rpm", 100); it != nil {
		t.Error("an operation with no items handed out an Item")
	}
	tr.End()
	if got := buf.String(); got != "" {
		t.Errorf("an empty operation printed %q, want nothing", got)
	}
}

func TestWrapPassesWritesThrough(t *testing.T) {
	tr, buf := newTestTracker(t)
	tr.Begin("upload", 1, 100)
	w := tr.Wrap(buf)
	if _, err := io.WriteString(w, "staged one.rpm\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := buf.String(); got != "staged one.rpm\n" {
		t.Errorf("wrapped write produced %q, want the line unchanged", got)
	}
}

func TestRenderedLinesFitTheTerminal(t *testing.T) {
	tr, _ := newTestTracker(t)
	tr.tty = true
	tr.Begin("copy", 57, 6_000_000_000)
	it := tr.Item("a-very-long-package-name-that-will-not-fit-on-one-line-1.2.3-1.el9.x86_64.rpm", 129_000_000)
	it.Phase("get")
	it.Add(62_000_000)
	// A second transfer in flight is reported as a count, not a third line.
	other := tr.Item("another.rpm", 1000)
	defer other.Done()

	tr.mu.Lock()
	tr.rate.observe(tr.started.Add(time.Second), 62_000_000)
	lines := tr.renderLocked()
	tr.mu.Unlock()

	if len(lines) != 2 {
		t.Fatalf("rendered %d lines, want 2 (the current transfer and the total)", len(lines))
	}
	for _, l := range lines {
		if len([]rune(l)) > 80 {
			t.Errorf("line is %d columns wide, want at most 80:\n%s", len([]rune(l)), l)
		}
	}
	if !strings.Contains(lines[0], "get") || !strings.Contains(lines[0], "(+1 more)") {
		t.Errorf("transfer line does not name the phase and the others in flight:\n%s", lines[0])
	}
	if !strings.Contains(lines[1], "0/57") {
		t.Errorf("total line does not report the item count:\n%s", lines[1])
	}
}

// transferred and completed read the operation totals under the lock, for tests.
func (t *Tracker) transferred() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.doneBytes
}

func (t *Tracker) completed() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.doneItems
}
