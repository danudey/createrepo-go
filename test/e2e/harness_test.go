//go:build e2e

// Package e2e contains end-to-end tests that drive the createrepo-go CLI binary
// as a subprocess against real storage backends (local disk, a freshly started
// MinIO instance for S3, a local sshd for SFTP, a fake-gcs-server container for
// GCS, and a read-only HTTP server). A single comprehensive scenario suite is
// run against every writable backend so behavior is validated identically
// regardless of where the repository lives.
//
// Run with:
//
//	go test -tags e2e ./test/e2e/...
//
// Backends whose dependencies are absent (docker, minio, sshd, ...) skip
// themselves rather than fail, so the suite degrades gracefully.
package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Harness abstracts one storage backend under test. A harness is started once
// per backend (Start), then each scenario asks for a fresh, isolated repository
// location (RepoURL). Exists/ReadAll let scenarios inspect stored objects
// directly (out of band of the CLI) to assert on blob placement and GC.
type Harness interface {
	// Name is the backend's short identifier (used as the subtest name).
	Name() string
	// Start brings the backend up (servers, directories, credentials) and
	// registers teardown with t.Cleanup. It calls t.Skip if the backend's
	// dependencies are unavailable.
	Start(t *testing.T)
	// RepoURL returns a brand-new, empty repository location for one scenario.
	RepoURL(t *testing.T) string
	// Env returns extra environment variables the CLI subprocess needs to reach
	// this backend (credentials, endpoints, SSH agent socket, ...).
	Env() []string
	// Fetch reads a repo-relative object without a *testing.T, returning
	// (data, found, err). It is safe to call from non-test goroutines (e.g. an
	// HTTP handler fronting the repository for a container client).
	Fetch(repoURL, relpath string) ([]byte, bool, error)
	// Exists reports whether a repo-relative object is present in storage.
	Exists(t *testing.T, repoURL, relpath string) bool
	// ReadAll returns the full contents of a repo-relative object.
	ReadAll(t *testing.T, repoURL, relpath string) []byte
	// Remove deletes a repo-relative object out of band (to simulate damage).
	Remove(t *testing.T, repoURL, relpath string)
	// WriteRaw replaces a repo-relative object's bytes out of band.
	WriteRaw(t *testing.T, repoURL, relpath string, data []byte)
	// RemoteHash reports whether the backend can validate an existing RPM by
	// checksum without downloading it (affects the expected "skip" reason).
	RemoteHash() bool
}

// ---- CLI subprocess runner -------------------------------------------------

// cliResult is the captured outcome of one CLI invocation.
type cliResult struct {
	stdout string
	stderr string
	err    error // non-nil if the process exited non-zero
}

func (r cliResult) combined() string { return r.stdout + r.stderr }

// runCLI executes the built binary. The given env entries are layered on top of
// the current process environment (later entries override earlier ones), so
// backends only need to specify the variables they change.
func runCLI(t *testing.T, env []string, args ...string) cliResult {
	return runCLIStdin(t, env, "", args...)
}

// runCLIStdin is runCLI with stdin fed from input (used to answer confirmation
// prompts).
func runCLIStdin(t *testing.T, env []string, input string, args ...string) cliResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Env = append(os.Environ(), env...)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	res := cliResult{stdout: out.String(), stderr: errb.String(), err: err}
	t.Logf("$ createrepo-go %s\n%s", strings.Join(args, " "), res.combined())
	return res
}

// ---- network / readiness utilities ----------------------------------------

// freePort returns an OS-assigned free TCP port on 127.0.0.1.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// waitFor polls fn until it returns nil or the deadline passes.
func waitFor(t *testing.T, what string, timeout time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = fn(); last == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %v", what, last)
}

// waitForTCP waits until a TCP connection to addr succeeds.
func waitForTCP(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	waitFor(t, "tcp "+addr, timeout, func() error {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return err
		}
		c.Close()
		return nil
	})
}

// haveExec reports whether all named executables are on PATH.
func haveExec(names ...string) (string, bool) {
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			return n, false
		}
	}
	return "", true
}

// uniq builds a short, unique-ish path segment from a test name.
func uniq(t *testing.T) string {
	name := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	return fmt.Sprintf("%s-%d", name, time.Now().UnixNano()%1_000_000)
}
