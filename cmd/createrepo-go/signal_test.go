package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Ctrl-C during package signing must not leave the passphrase file behind.
// Before signals were caught the process died on the spot and no deferred
// cleanup ran, so the plaintext passphrase stayed in $TMPDIR.
func TestInterruptedSigningRemovesThePassphraseFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell-script rpmsign and SIGINT")
	}
	if testing.Short() {
		t.Skip("builds the binary")
	}
	work := t.TempDir()
	bin := filepath.Join(work, "createrepo-go")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	// A stand-in rpmsign that reports it has started, then hangs as if gpg
	// were waiting on something.
	fake := filepath.Join(work, "fakebin")
	started := filepath.Join(work, "started")
	os.Mkdir(fake, 0o755)
	script := "#!/bin/sh\ncase \"$1\" in --help) exit 0;; esac\ntouch " + started + "\nsleep 30 &\nwait\n"
	if err := os.WriteFile(filepath.Join(fake, "rpmsign"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(work, "tmp")
	os.Mkdir(tmp, 0o700)

	repoDir := filepath.Join(work, "repo")
	cmd := exec.Command(bin, "add", repoDir, abs(t, helloRPM),
		"--sign-packages", "--gpg-key-id", "ABCDEF0123456789", "--gpg-passphrase", "hunter2")
	cmd.Env = append(os.Environ(), "PATH="+fake+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+tmp)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("rpmsign never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "cr-pass-*")); len(left) == 0 {
		cmd.Process.Kill()
		t.Fatal("no passphrase file while signing; the test is not exercising the cleanup")
	}

	cmd.Process.Signal(syscall.SIGINT)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("an interrupted add exited successfully")
		}
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		t.Fatal("add did not exit after SIGINT")
	}

	var left []string
	for _, pattern := range []string{"cr-pass-*", "cr-sign-*"} {
		m, _ := filepath.Glob(filepath.Join(tmp, pattern))
		left = append(left, m...)
	}
	if len(left) > 0 {
		t.Errorf("an interrupted add left %s behind", strings.Join(left, ", "))
	}
}

func abs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
