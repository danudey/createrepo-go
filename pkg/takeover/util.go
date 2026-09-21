package takeover

import (
	"fmt"
	"os"
	"sort"
)

// sortStrings sorts a slice of strings in place.
func sortStrings(s []string) { sort.Strings(s) }

// plural renders a count with the singular or plural form of a noun, so
// findings read as sentences rather than as "1 packages".
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// humanBytes formats a byte count with a binary (KiB/MiB/...) suffix.
func humanBytes(n int64) string {
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

// removeFile deletes a temporary file, ignoring failures: it is always cleanup
// of a download this process made and no longer needs.
func removeFile(path string) { os.Remove(path) }

// shortSum abbreviates a checksum to its leading digits, which is enough to
// tell two apart in a report.
func shortSum(sum string) string {
	if len(sum) <= 12 {
		return sum
	}
	return sum[:12] + "…"
}
