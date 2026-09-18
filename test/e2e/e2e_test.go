//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// binPath is the freshly built CLI under test; gRPMs holds the shared fixtures.
var (
	binPath string
	gRPMs   rpmSet
)

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "cr-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: mktemp:", err)
		os.Exit(1)
	}

	// Build the CLI binary from the module root.
	binPath = filepath.Join(tmp, "createrepo-go")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/createrepo-go")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: build CLI: %v\n%s\n", err, out)
		os.Exit(1)
	}

	// Locate the reference RPMs and build the bumped hello (if rpmbuild exists).
	rpmTmp := filepath.Join(tmp, "rpms")
	if err := os.MkdirAll(rpmTmp, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
	gRPMs, err = buildRPMs(rpmTmp)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: build RPM fixtures:", err)
		os.Exit(1)
	}

	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

// TestE2E runs the full scenario suite against each writable backend. Backends
// whose dependencies are missing skip themselves.
func TestE2E(t *testing.T) {
	harnesses := []Harness{
		&localHarness{},
		&s3Harness{},
		&sftpHarness{},
		&gcsHarness{},
	}
	for _, h := range harnesses {
		t.Run(h.Name(), func(t *testing.T) {
			h.Start(t) // may t.Skip
			for _, sc := range scenarios {
				t.Run(sc.name, func(t *testing.T) {
					if sc.needsNewer && !gRPMs.haveNewer() {
						t.Skip("rpmbuild unavailable; no newer hello build for this scenario")
					}
					if sc.needsDep && !gRPMs.haveDep() {
						t.Skip("rpmbuild unavailable; no dependency fixtures for this scenario")
					}
					c := newTctx(t, h)
					sc.run(c, gRPMs)
				})
			}
		})
	}
}

// TestE2EReadOnlyHTTP publishes a repository to local disk and then exercises
// the read-only HTTP backend against it (list/verify), and asserts that write
// operations are rejected.
func TestE2EReadOnlyHTTP(t *testing.T) {
	dir := t.TempDir()

	// Publish a two-package repo on local disk.
	if res := runCLI(t, nil, "add", dir, gRPMs.hello, gRPMs.libfoo); res.err != nil {
		t.Fatalf("seed local repo: %v\n%s", res.err, res.combined())
	}

	// Serve it over HTTP.
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	httpURL := srv.URL + "/"

	// list and verify must work read-only over HTTP.
	listRes := runCLI(t, nil, "list", httpURL)
	if listRes.err != nil {
		t.Fatalf("list over http: %v\n%s", listRes.err, listRes.combined())
	}
	if !strings.Contains(listRes.stdout, "2 package(s)") {
		t.Errorf("http list did not report 2 packages:\n%s", listRes.stdout)
	}
	for _, want := range []string{"hello", "libfoo"} {
		if !strings.Contains(listRes.stdout, want) {
			t.Errorf("http list missing %q:\n%s", want, listRes.stdout)
		}
	}
	if verRes := runCLI(t, nil, "verify", httpURL); verRes.err != nil {
		t.Errorf("verify over http failed: %v\n%s", verRes.err, verRes.combined())
	}

	// check must fully validate the repository over HTTP, including a fetch-level
	// download of every package.
	checkRes := runCLI(t, nil, "check", "--arch", "any", "--level", "fetch", httpURL)
	if checkRes.err != nil {
		t.Errorf("check over http failed: %v\n%s", checkRes.err, checkRes.combined())
	}
	if !strings.Contains(checkRes.stdout, "RESULT: OK") {
		t.Errorf("check over http did not report OK:\n%s", checkRes.combined())
	}

	// Writes must be rejected on the read-only backend.
	addRes := runCLI(t, nil, "add", httpURL, gRPMs.hello)
	if addRes.err == nil {
		t.Errorf("add over http unexpectedly succeeded; expected read-only rejection\n%s", addRes.combined())
	}
}
