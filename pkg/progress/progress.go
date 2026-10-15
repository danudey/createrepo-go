// Package progress renders optional progress reporting for the parts of a
// repository operation that take real time: uploading RPMs, downloading them
// again to hash or re-sign them, and copying a repository from one backend to
// another.
//
// A Tracker shows two things at once — what each transfer in flight is doing,
// with a bar apiece, and how far through the whole operation that leaves it.
// Both are driven by bytes actually read from or written to the backend, so a
// stalled transfer looks stalled rather than merely slow.
//
// A nil *Tracker is a complete no-op: every method is safe to call on one, so
// a caller that was given no tracker (progress not asked for) needs no nil
// checks of its own. Every method is also safe to call from several goroutines
// at once, which the concurrent verify and check paths rely on.
package progress

import (
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vbauerster/mpb/v8"
	"golang.org/x/term"
)

const (
	// paintInterval is how often the display is redrawn on a terminal. It is
	// short enough that the rate and ETA feel live, and long enough that a fast
	// local copy does not spend its time painting.
	paintInterval = 100 * time.Millisecond
	// plainInterval is how often a single summary line is printed when the
	// output is not a terminal (a log file, a CI job). Bars would be unreadable
	// there, but silence during a multi-hour mirror is worse.
	plainInterval = 30 * time.Second

	// defaultCols is assumed when the terminal's width cannot be determined,
	// and maxCols keeps the display readable on a very wide one.
	defaultCols = 80
	maxCols     = 120
	minCols     = 40
)

// Tracker reports the progress of one operation: an overall bar covering every
// item it was told about, plus a bar for each item currently transferring.
type Tracker struct {
	out   io.Writer
	tty   bool
	width func() int
	// plainEvery is how often the non-terminal fallback prints a summary line.
	plainEvery time.Duration

	mu sync.Mutex
	// gen identifies the current operation. An Item left over from an earlier
	// one is ignored rather than corrupting the totals.
	gen        uint64
	begun      bool
	op         string
	totalItems int
	totalBytes int64
	doneItems  int
	doneBytes  int64
	active     []*Item
	started    time.Time
	rate       rateMeter
	lastPlain  time.Time

	// p draws the bars while an operation is open on a terminal, and total is
	// the operation's own bar within it. Both are nil otherwise.
	p     *mpb.Progress
	total *mpb.Bar
	shown shown
}

// New returns a Tracker writing its display to w, or nil when enabled is false.
// A nil Tracker is inert, so the caller can pass the result around without
// caring which it got. w is normally os.Stderr: the display is not output, and
// a caller piping the command's results somewhere should not receive it.
func New(w io.Writer, enabled bool) *Tracker {
	if !enabled || w == nil {
		return nil
	}
	t := &Tracker{
		out:        w,
		plainEvery: plainInterval,
		width:      func() int { return defaultCols },
	}
	if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fd := int(f.Fd())
		t.tty = true
		t.width = func() int {
			cols, _, err := term.GetSize(fd)
			switch {
			case err != nil || cols < minCols:
				return defaultCols
			case cols > maxCols:
				return maxCols
			default:
				return cols
			}
		}
	}
	return t
}

// Begin starts an operation: items transfers totalling bytes bytes, described
// by op ("upload", "copy", ...). An operation with nothing in it is ignored, so
// callers need not check for empty work. Beginning while another operation is
// still open ends that one first.
//
// bytes is what the operation expects to move, which for a copy is each object
// twice: once fetched from the source and once written to the destination. Work
// that turns out not to be needed is taken back off the totals with Skip.
func (t *Tracker) Begin(op string, items int, bytes int64) {
	if t == nil || items <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.begun {
		t.finishLocked()
	}
	now := time.Now()
	t.gen++
	t.begun = true
	t.op, t.totalItems, t.totalBytes = op, items, bytes
	t.doneItems, t.doneBytes = 0, 0
	t.active = nil
	t.started, t.lastPlain = now, now
	t.rate.reset(now)
	if t.tty {
		t.startDisplayLocked()
	}
	t.publishLocked()
	go t.loop(t.gen)
}

// End closes the current operation, leaving a one-line summary of what it moved
// behind. It is safe to call when no operation is open.
func (t *Tracker) End() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.finishLocked()
}

// finishLocked ends the current operation. The painter goroutine notices on its
// next tick that its generation is stale and exits on its own, so nothing here
// waits on it.
func (t *Tracker) finishLocked() {
	if !t.begun {
		return
	}
	t.begun = false
	t.gen++
	t.stopDisplayLocked()
	if t.doneItems > 0 || t.doneBytes > 0 {
		fmt.Fprintln(t.out, t.summaryLocked(time.Now()))
	}
	t.active = nil
}

// Skip takes work back off the operation's totals: items that turned out to be
// already present, or otherwise not to need transferring. Without it the bars
// would report a repository as half-copied when in truth it was all there.
func (t *Tracker) Skip(items int, bytes int64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.begun {
		return
	}
	t.totalItems -= items
	t.totalBytes -= bytes
	if t.totalItems < t.doneItems {
		t.totalItems = t.doneItems
	}
	if t.totalBytes < t.doneBytes {
		t.totalBytes = t.doneBytes
	}
	t.publishLocked()
}

// Wrap returns w routed through the display, so that a line printed while bars
// are on screen lands above them instead of on top of them. It returns w
// unchanged when there is no tracker.
//
// Only a writer that shares the display's screen is routed through it: the
// display's own output, or another terminal (stdout and stderr both on the same
// tty). Output going anywhere else, such as stdout piped to a file, is written
// straight through, so the file receives exactly what it would have without
// progress.
func (t *Tracker) Wrap(w io.Writer) io.Writer {
	if t == nil {
		return w
	}
	return &wrappedWriter{t: t, w: w}
}

type wrappedWriter struct {
	t *Tracker
	w io.Writer
}

func (ww *wrappedWriter) Write(b []byte) (int, error) {
	ww.t.mu.Lock()
	p := ww.t.p
	ww.t.mu.Unlock()
	if p != nil && (ww.w == ww.t.out || isTerminal(ww.w)) {
		// mpb prints the line above the bars on its next refresh. Once the
		// display has shut down it refuses, and the line goes straight out.
		if n, err := p.Write(b); err == nil {
			return n, nil
		}
	}
	return ww.w.Write(b)
}

// isTerminal reports whether w is a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// Item starts one transfer within the current operation and returns a handle
// for reporting its progress. The returned *Item is nil when no operation is
// open, which its methods tolerate. size may be 0 or negative when it is not
// known in advance; the item then reports bytes moved without a bar.
func (t *Tracker) Item(name string, size int64) *Item {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.begun {
		return nil
	}
	return t.startLocked(name, size, false)
}

// Aside starts a transfer the operation's totals do not count: work the
// operation did not plan for, such as re-reading a file that was just written.
// It gets a bar while it runs, but neither its bytes nor its completion move
// the overall figures, so they keep describing the work Begin announced.
func (t *Tracker) Aside(name string, size int64) *Item {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.begun {
		return nil
	}
	return t.startLocked(name, size, true)
}

// startLocked creates and displays an item. Called with t.mu held.
func (t *Tracker) startLocked(name string, size int64, aside bool) *Item {
	it := &Item{t: t, gen: t.gen, name: name, size: size, aside: aside}
	op := t.op
	it.label.Store(&op)
	it.bar = t.itemBarLocked(it)
	t.active = append(t.active, it)
	return it
}

// Count credits items that finished without moving any bytes through this
// process — a server-side relocation, a deletion — so the operation's item
// count covers all of its work rather than only its transfers.
func (t *Tracker) Count(items int) {
	if t == nil || items <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.begun {
		return
	}
	t.doneItems += items
	t.publishLocked()
}

// Item is one transfer inside an operation.
type Item struct {
	t   *Tracker
	gen uint64

	name string
	size int64
	// aside is set for an item started with Aside, whose bytes and completion
	// the operation's totals ignore.
	aside bool
	// label is the phase shown on the item's bar. It is read by the display
	// without t.mu, so it is atomic.
	label atomic.Pointer[string]
	// bar is the item's bar, or nil when there is no terminal display.
	bar *mpb.Bar

	// The fields below are guarded by t.mu.
	done  int64
	ended bool
}

// stale reports whether the item belongs to an operation that has since ended.
// Called with t.mu held.
func (i *Item) stale() bool { return i.ended || i.t.gen != i.gen || !i.t.begun }

// Add counts n bytes transferred for this item and for the operation.
func (i *Item) Add(n int64) {
	if i == nil || n <= 0 {
		return
	}
	i.t.mu.Lock()
	defer i.t.mu.Unlock()
	if i.stale() {
		return
	}
	i.done += n
	if !i.aside {
		i.t.doneBytes += n
	}
	i.setBarLocked()
}

// setBarLocked shows the item's progress, and the operation's, on their bars.
// Called with t.mu held.
func (i *Item) setBarLocked() {
	if i.bar != nil {
		i.bar.SetCurrent(i.done)
	}
	i.t.publishLocked()
}

// Phase labels what is being done to the item now — "get" then "put" for a
// copy, say — and restarts its bar. Bytes already transferred stay counted
// towards the operation, because they really did move.
func (i *Item) Phase(label string) {
	if i == nil {
		return
	}
	i.t.mu.Lock()
	defer i.t.mu.Unlock()
	if i.stale() {
		return
	}
	i.label.Store(&label)
	i.done = 0
	i.setBarLocked()
}

// rewind un-counts the bytes read from this item so far. It is called when a
// wrapped reader is seeked back to the start, which means those bytes are about
// to be read a second time: an object store hashing a body before sending it,
// or the SDK replaying a request after a retry. Counting both passes would
// report twice the traffic that actually crosses the network.
func (i *Item) rewind() {
	if i == nil {
		return
	}
	i.t.mu.Lock()
	defer i.t.mu.Unlock()
	if i.stale() {
		return
	}
	if !i.aside {
		i.t.doneBytes -= i.done
	}
	i.done = 0
	i.setBarLocked()
}

// Done marks the item finished. Calling it more than once is harmless, so it
// can be deferred and also called explicitly on the success path.
func (i *Item) Done() {
	if i == nil {
		return
	}
	i.t.mu.Lock()
	defer i.t.mu.Unlock()
	if i.stale() {
		return
	}
	i.ended = true
	if !i.aside {
		i.t.doneItems++
	}
	for n, other := range i.t.active {
		if other == i {
			i.t.active = append(i.t.active[:n], i.t.active[n+1:]...)
			break
		}
	}
	if i.bar != nil {
		i.bar.Abort(true)
	}
	i.t.publishLocked()
}

// Reader returns r wrapped so every byte read from it counts towards this item
// and the operation.
//
// When r is an io.ReadSeeker the wrapper is one too. That matters for the S3
// and GCS backends, which require a seekable body and read it twice: once to
// hash it, then again to send it. Seeking back to the start un-counts what was
// read (see rewind), so the display restarts the item rather than reporting
// twice the bytes the network actually carried.
func (i *Item) Reader(r io.Reader) io.Reader {
	if i == nil {
		return r
	}
	// Wrapping also hides any io.ReaderFrom fast path the destination could
	// have used on the underlying file, which is unavoidable: bytes that move
	// inside the kernel cannot be counted. Progress is opt-in for that reason.
	if rs, ok := r.(io.ReadSeeker); ok {
		return &countingSeeker{countingReader: countingReader{item: i, r: rs}, s: rs}
	}
	return &countingReader{item: i, r: r}
}

type countingReader struct {
	item *Item
	r    io.Reader
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.item.Add(int64(n))
	return n, err
}

type countingSeeker struct {
	countingReader
	s io.Seeker
}

func (c *countingSeeker) Seek(offset int64, whence int) (int64, error) {
	pos, err := c.s.Seek(offset, whence)
	if err == nil && pos == 0 {
		c.item.rewind()
	}
	return pos, err
}

// Writer returns w wrapped so every byte written to it counts towards this item
// and the operation: for a download streamed into a hash or a temporary file.
func (i *Item) Writer(w io.Writer) io.Writer {
	if i == nil {
		return w
	}
	return &countingWriter{item: i, w: w}
}

type countingWriter struct {
	item *Item
	w    io.Writer
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.item.Add(int64(n))
	return n, err
}

// loop keeps the transfer rate current until the operation it was started for
// ends, and on a non-terminal prints the periodic summary line.
func (t *Tracker) loop(gen uint64) {
	interval := paintInterval
	if !t.tty {
		interval = time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for range tick.C {
		if !t.paint(gen) {
			return
		}
	}
}

// paint refreshes the current state, reporting false once this operation is
// over.
func (t *Tracker) paint(gen uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.begun || t.gen != gen {
		return false
	}
	now := time.Now()
	t.rate.observe(now, t.doneBytes)
	t.publishLocked()
	if !t.tty {
		// No cursor to move: report one summary line now and then instead.
		if now.Sub(t.lastPlain) >= t.plainEvery {
			t.lastPlain = now
			fmt.Fprintln(t.out, t.summaryLocked(now))
		}
		return true
	}
	// mpb repaints the bars on its own schedule.
	return true
}
