package sign

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Cancelling a signing (Ctrl-C reaches it as a cancelled context) must stop
// rpmsign promptly and still remove the passphrase file on the way out.
func TestCancelledSigningStopsAndCleansUp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake rpmsign is a shell script")
	}
	bin := t.TempDir()
	// The sleep stands in for gpg: a child that outlives rpmsign and holds
	// its output open.
	script := "#!/bin/sh\nsleep 30 &\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "rpmsign"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	rpmFile := abs(t, referenceRPM)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := NewPackageSignerKeyID("ABCDEF0123456789", awkwardPassphrase).SignFileContext(ctx, rpmFile)
	if err == nil {
		t.Fatal("a cancelled signing succeeded")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("signing took %s to stop after cancellation", d)
	}
	left, _ := filepath.Glob(filepath.Join(tmp, "cr-*"))
	if len(left) > 0 {
		t.Errorf("a cancelled signing left %v behind", left)
	}
}
