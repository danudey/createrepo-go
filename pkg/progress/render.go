package progress

import (
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

// The terminal display is drawn by mpb: one bar per transfer in flight, and a
// bar for the operation as a whole pinned beneath them. Every bar is created
// with no total of its own (see newBar), so mpb never decides by itself that a
// bar is finished; the Tracker says when, which is what lets an item's bar be
// restarted for its next phase.

// Use box drawing characters for the progress bar because it looks nicer.
var barStyle = mpb.BarStyle().Lbound(" ").Filler("━").Tip("━").Padding(" ").Rbound(" ")

// totalPriority keeps the operation's bar below every transfer's bar.
const totalPriority = math.MaxInt

// startDisplayLocked creates the mpb container and the operation's bar for the
// operation just begun. Called with t.mu held.
func (t *Tracker) startDisplayLocked() {
	t.p = mpb.New(
		mpb.WithOutput(t.out),
		mpb.WithWidth(t.width()),
		mpb.WithRefreshRate(paintInterval),
		// The Tracker has already decided the output is a terminal, which mpb
		// confirms for itself on an *os.File. This keeps it drawing when it
		// cannot tell, such as into a test buffer. (ForceTTY would not do: it
		// makes mpb ask the writer for a terminal size it does not have.)
		mpb.WithAutoRefresh(),
	)
	op := t.op
	t.total = t.newBar(t.totalBytes,
		mpb.BarPriority(totalPriority),
		mpb.PrependDecorators(decor.Name(label(op))),
		mpb.AppendDecorators(decor.Any(t.shown.overallMeasure)),
	)
}

// stopDisplayLocked takes every bar off the screen and waits for mpb to finish
// drawing, so that whatever is written next lands on a clean line. Called with
// t.mu held; the bars' decorators never take t.mu, so waiting here is safe.
func (t *Tracker) stopDisplayLocked() {
	if t.p == nil {
		return
	}
	for _, it := range t.active {
		it.bar.Abort(true)
	}
	t.total.Abort(true)
	t.p.Wait()
	t.p, t.total = nil, nil
}

// newBar adds a bar that shows total as its size but never completes on its
// own: mpb treats a bar created with a zero total as open-ended, and SetTotal
// without forceComplete changes what is shown without changing that.
func (t *Tracker) newBar(total int64, opts ...mpb.BarOption) *mpb.Bar {
	b := t.p.New(0, barStyle, append(opts, mpb.BarRemoveOnComplete())...)
	if total > 0 {
		b.SetTotal(total, false)
	}
	return b
}

// itemBarLocked adds the bar for one transfer. Called with t.mu held.
func (t *Tracker) itemBarLocked(it *Item) *mpb.Bar {
	if t.p == nil {
		return nil
	}
	width := t.width()
	name := pad(it.name, nameCols(width))
	return t.newBar(it.size,
		mpb.PrependDecorators(
			decor.Any(func(decor.Statistics) string { return label(*it.label.Load()) }),
			decor.Name(name+"  "),
		),
		mpb.AppendDecorators(decor.Any(func(s decor.Statistics) string {
			return itemMeasure(s.Current, it.size)
		}, decor.WC{W: measureWidth, C: decor.DindentRight})),
	)
}

// itemMeasure is what follows a transfer's bar: how far through it is, or just
// the bytes moved when its size is not known.
func itemMeasure(done, size int64) string {
	if size <= 0 {
		return " " + Bytes(done)
	}
	return fmt.Sprintf(" %3.0f%%  %s/%s", fraction(done, size)*100, Bytes(done), Bytes(size))
}

// shown is the part of the operation's state the operation's bar displays that
// mpb does not track itself. mpb draws from its own goroutines, so this is kept
// in atomics rather than behind t.mu: a decorator that took t.mu could deadlock
// against stopDisplayLocked, which holds it while waiting for mpb.
type shown struct {
	doneItems, totalItems atomic.Int64
	doneBytes, totalBytes atomic.Int64
	rate                  atomicFloat
}

// atomicFloat is a float64 that can be read and written from any goroutine.
type atomicFloat struct{ bits atomic.Uint64 }

func (f *atomicFloat) Load() float64   { return math.Float64frombits(f.bits.Load()) }
func (f *atomicFloat) Store(v float64) { f.bits.Store(math.Float64bits(v)) }

// overallMeasure renders the operation as a whole: how much of the work is done,
// how fast it is going, and how long the rest should take.
func (s *shown) overallMeasure(decor.Statistics) string {
	done, total := s.doneBytes.Load(), s.totalBytes.Load()
	var parts []string
	if total > 0 {
		parts = append(parts, fmt.Sprintf("%3.0f%%", fraction(done, total)*100))
	}
	parts = append(parts, fmt.Sprintf("%d/%d", s.doneItems.Load(), s.totalItems.Load()))
	if total > 0 {
		parts = append(parts, fmt.Sprintf("%s/%s", Bytes(done), Bytes(total)))
	}
	if rate := s.rate.Load(); rate > 0 {
		parts = append(parts, Bytes(int64(rate))+"/s")
		if left := total - done; left > 0 {
			parts = append(parts, "ETA "+Duration(time.Duration(float64(left)/rate*float64(time.Second))))
		}
	}
	return " " + strings.Join(parts, "  ")
}

// publishLocked copies the operation's totals to the display. Called with t.mu
// held, after anything that changes them.
func (t *Tracker) publishLocked() {
	t.shown.doneItems.Store(int64(t.doneItems))
	t.shown.totalItems.Store(int64(t.totalItems))
	t.shown.doneBytes.Store(t.doneBytes)
	t.shown.totalBytes.Store(t.totalBytes)
	t.shown.rate.Store(t.rate.perSecond())
	if t.total != nil {
		t.total.SetTotal(max(t.totalBytes, 0), false)
		t.total.SetCurrent(max(t.doneBytes, 0))
	}
}

// summaryLocked is the single line printed when an operation ends, and
// periodically in place of the bars when the output is not a terminal.
func (t *Tracker) summaryLocked(now time.Time) string {
	elapsed := now.Sub(t.started)
	line := fmt.Sprintf("%s: %d/%d complete, %s", t.op, t.doneItems, t.totalItems, Bytes(t.doneBytes))
	if t.totalBytes > 0 {
		line += "/" + Bytes(t.totalBytes)
	}
	line += fmt.Sprintf(" in %s", Duration(elapsed))
	if secs := elapsed.Seconds(); secs > 0 && t.doneBytes > 0 {
		line += fmt.Sprintf(" (%s/s)", Bytes(int64(float64(t.doneBytes)/secs)))
	}
	return line
}

// Every bar starts with an indent and a word in a column of its own — the phase
// for a transfer, the operation for the total — so that the bars line up.
const (
	indent = "  "
	// labelWidth fits the longest label, "download", and the space after it.
	labelWidth = 9
	// minName is the narrowest useful name column.
	minName = 8
	// barRoom is what a name column leaves for the bar itself.
	barRoom = 24
	// measureWidth is the room reserved for what follows a transfer's bar: a
	// percentage and a pair of byte figures. Reserving it keeps the bar in one
	// place as those figures change length.
	measureWidth = 30
)

// label renders the word a bar opens with.
func label(word string) string {
	return fmt.Sprintf("%s%-*s", indent, labelWidth, word)
}

// pad renders a name in a column of exactly n characters, shortening it if it
// does not fit.
func pad(name string, n int) string {
	return fmt.Sprintf("%-*s", n, clip(name, n))
}

// nameCols is the width of the name column at this terminal width. It depends
// on the width alone, so every transfer's bar starts in the same column.
func nameCols(width int) int {
	return max(width-len(indent)-labelWidth-2-barRoom-measureWidth, minName)
}

// fraction is done/total, clamped to [0,1] so an unexpected overshoot cannot
// report more than all of it.
func fraction(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	f := float64(done) / float64(total)
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	default:
		return f
	}
}

// clip shortens s to at most n characters, marking where it was cut.
func clip(s string, n int) string {
	r := []rune(s)
	if n <= 0 {
		return ""
	}
	if len(r) <= n {
		return s
	}
	if n <= 3 {
		return string(r[:n])
	}
	return string(r[:n-3]) + "..."
}

// Bytes formats a byte count with a binary (KiB/MiB/...) suffix.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Duration formats a duration as a short, human-readable interval: seconds for
// anything under a minute, then minutes, then hours.
func Duration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// rateMeter estimates the current transfer rate. It is an exponential moving
// average rather than a running total, so the figure reflects what the transfer
// is doing now and not what it averaged half an hour ago.
type rateMeter struct {
	last  time.Time
	bytes int64
	ema   float64
	seen  bool
}

// window is the time constant of the average: a change in speed is mostly
// reflected within this much time.
const rateWindow = 3 * time.Second

func (m *rateMeter) reset(now time.Time) {
	m.last, m.bytes, m.ema, m.seen = now, 0, 0, false
}

// observe records the running byte total at time now.
func (m *rateMeter) observe(now time.Time, total int64) {
	elapsed := now.Sub(m.last)
	if elapsed <= 0 {
		return
	}
	// A rewind un-counts bytes, so the total can go backwards; treat such an
	// interval as idle rather than letting it drive the rate negative.
	moved := max(total-m.bytes, 0)
	sample := float64(moved) / elapsed.Seconds()
	m.last, m.bytes = now, total
	if !m.seen {
		m.ema, m.seen = sample, true
		return
	}
	// Standard EMA smoothing factor for an elapsed interval against the window.
	alpha := elapsed.Seconds() / (rateWindow.Seconds() + elapsed.Seconds())
	m.ema += alpha * (sample - m.ema)
}

// perSecond returns the current estimated rate in bytes per second.
func (m *rateMeter) perSecond() float64 {
	if !m.seen || m.ema < 0 {
		return 0
	}
	return m.ema
}
