package progress

import (
	"fmt"
	"strings"
	"time"
)

// The display is drawn in place on a terminal, which needs three standard
// terminal control sequences. They are written only when the output really is a
// terminal (see Tracker.tty); the fallback path prints plain lines instead.
const (
	// csi introduces a control sequence.
	csi = "\x1b["
	// cursorUpFmt moves the cursor up n rows.
	cursorUpFmt = csi + "%dA"
	// clearBelow erases from the cursor to the end of the screen.
	clearBelow = csi + "0J"
	// lineStart returns the cursor to column one.
	lineStart = "\r"
)

const (
	// barWidth is the width of a bar on a normal terminal; narrowBar is used
	// when the terminal is too narrow to spare the room.
	barWidth   = 24
	narrowBar  = 12
	narrowCols = 96
)

// drawLocked repaints the display in place. Called with t.mu held.
func (t *Tracker) drawLocked() {
	t.eraseLocked()
	for _, line := range t.renderLocked() {
		fmt.Fprintln(t.out, line)
		t.lines++
	}
}

// eraseLocked removes the display from the screen, leaving the cursor where it
// was before the display was drawn, so whatever is written next lands on a
// clean line. Called with t.mu held.
func (t *Tracker) eraseLocked() {
	if !t.tty || t.lines == 0 {
		return
	}
	fmt.Fprintf(t.out, cursorUpFmt+lineStart+clearBelow, t.lines)
	t.lines = 0
}

// renderLocked builds the display: a line for the transfer currently in front
// of us (when there is one) and a line for the operation as a whole.
func (t *Tracker) renderLocked() []string {
	width := t.width()
	var lines []string
	if len(t.active) > 0 {
		lines = append(lines, t.itemLine(t.active[0], len(t.active)-1, width))
	}
	return append(lines, t.overallLine(width))
}

// itemLine renders the transfer in progress. With several running at once (a
// concurrent verify or check) the oldest one is shown and the rest are counted,
// which keeps the display two lines tall however wide the fan-out is.
func (t *Tracker) itemLine(it *Item, others, width int) string {
	suffix := ""
	if others > 0 {
		suffix = fmt.Sprintf("  (+%d more)", others)
	}
	cols := barCols(width)
	room := nameCols(width, cols) - len(suffix)

	measure := Bytes(it.done)
	if it.size > 0 {
		frac := fraction(it.done, it.size)
		measure = fmt.Sprintf("%s %3.0f%%  %s/%s", bar(frac, cols), frac*100, Bytes(it.done), Bytes(it.size))
	}
	return clip(label(it.phase)+pad(it.name, room)+"  "+measure+suffix, width)
}

// overallLine renders the operation as a whole: how much of the work is done,
// how fast it is going, and how long the rest should take.
func (t *Tracker) overallLine(width int) string {
	var parts []string
	if t.totalBytes > 0 {
		frac := fraction(t.doneBytes, t.totalBytes)
		parts = append(parts, fmt.Sprintf("%s %3.0f%%", bar(frac, barCols(width)), frac*100))
	}
	parts = append(parts, fmt.Sprintf("%d/%d", t.doneItems, t.totalItems))
	if t.totalBytes > 0 {
		parts = append(parts, fmt.Sprintf("%s/%s", Bytes(t.doneBytes), Bytes(t.totalBytes)))
	}
	if rate := t.rate.perSecond(); rate > 0 {
		parts = append(parts, Bytes(int64(rate))+"/s")
		if eta, ok := t.etaLocked(rate); ok {
			parts = append(parts, "ETA "+Duration(eta))
		}
	}
	// The operation's own line is labelled with the operation, so the bar can
	// start right after it instead of past a name column.
	return clip(label(t.op)+strings.Join(parts, "  "), width)
}

// etaLocked estimates the time left at the current rate. It reports false when
// there is nothing left to transfer or nothing to base an estimate on.
func (t *Tracker) etaLocked(rate float64) (time.Duration, bool) {
	left := t.totalBytes - t.doneBytes
	if left <= 0 || rate <= 0 {
		return 0, false
	}
	return time.Duration(float64(left) / rate * float64(time.Second)), true
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

// Both display lines start with an indent and a word in a column of its own —
// the phase for a transfer, the operation for the total — so that the bars
// beside them line up.
const (
	indent     = "  "
	labelWidth = 7
	// minName is the narrowest useful name column.
	minName = 8
	// measureWidth is the room reserved for what follows a bar: a percentage
	// and a pair of byte figures. It is what a name column is sized against, so
	// that the bar keeps its column as those figures change length.
	measureWidth = 30
)

// label renders the word a display line opens with.
func label(word string) string {
	return fmt.Sprintf("%s%-*s", indent, labelWidth, word)
}

// pad renders a name in a column of exactly n characters, shortening it if it
// does not fit.
func pad(name string, n int) string {
	return fmt.Sprintf("%-*s", n, clip(name, n))
}

// nameCols is the width of the name column at this terminal width. It is
// derived from the width alone rather than from what is being displayed, so a
// transfer's bar stays in one place instead of sliding about as the byte
// figures beside it grow and shrink.
func nameCols(width, cols int) int {
	return max(width-len(indent)-labelWidth-2-cols-measureWidth, minName)
}

// barCols is how wide a bar may be at this terminal width.
func barCols(width int) int {
	if width < narrowCols {
		return narrowBar
	}
	return barWidth
}

// bar renders a proportion as a fixed-width ASCII bar. ASCII keeps it correct
// in any terminal and any locale, which a block-drawing character does not.
func bar(frac float64, cols int) string {
	filled := int(frac * float64(cols))
	switch {
	case filled < 0:
		filled = 0
	case filled > cols:
		filled = cols
	}
	return "[" + strings.Repeat("=", filled) + strings.Repeat("-", cols-filled) + "]"
}

// fraction is done/total, clamped to [0,1] so an unexpected overshoot cannot
// draw outside the bar.
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
