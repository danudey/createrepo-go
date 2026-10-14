//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// almaProfiles pairs an AlmaLinux major version with the createrepo-go --target
// profile a repository for that release should be built with. This is the exact
// compatibility matrix the README documents:
//
//	rhel8  -> gzip metadata, v4 (RSAHEADER) package signature   (rpm 4.14)
//	rhel9  -> zstd metadata, v4 (RSAHEADER) package signature   (rpm 4.16)
//	rhel10 -> zstd metadata, openpgp package signature          (rpm 4.19+)
var almaProfiles = []struct {
	ver     string // AlmaLinux major version / container tag
	profile string // createrepo-go --target
}{
	{"8", "rhel8"},
	{"9", "rhel9"},
	{"10", "rhel10"},
}

const clientKeyFile = "RPM-GPG-KEY-e2e" // virtual path served for the signing key

// TestE2EClients validates that real AlmaLinux dnf clients can consume the
// repositories createrepo-go produces. For every writable backend and every
// AlmaLinux version it builds a repository with the matching --target profile,
// fronts it over HTTP, and runs an AlmaLinux container that configures the repo
// and installs packages from it (with full gpgcheck + repo_gpgcheck when GPG
// tooling is available on the host).
func TestE2EClients(t *testing.T) {
	rt := containerRuntime(t)

	harnesses := []Harness{
		&localHarness{},
		&s3Harness{},
		&sftpHarness{},
		&gcsHarness{},
	}
	for _, h := range harnesses {
		t.Run(h.Name(), func(t *testing.T) {
			h.Start(t) // may t.Skip
			for _, ap := range almaProfiles {
				t.Run("alma"+ap.ver, func(t *testing.T) {
					image := "almalinux:" + ap.ver
					ensureImage(t, rt, image)
					clientInstallScenario(t, h, rt, image, ap.profile)
				})
			}
		})
	}
}

// clientInstallScenario builds a (signed, if possible) repository with the given
// profile, serves it, and runs an AlmaLinux client that installs from it.
func clientInstallScenario(t *testing.T, h Harness, rt, image, profile string) {
	c := newTctx(t, h)

	key, signed := maybeGPGKey(t)
	extra := map[string][]byte{}
	addFlags := []string{"--target", profile}
	if signed {
		addFlags = append(addFlags,
			"--sign-metadata", "--sign-packages", "--gpg-key-id", key.keyID)
		extra[clientKeyFile] = readFile(t, key.pubFile)
	}
	c.addWith(addFlags, gRPMs.hello, gRPMs.libfoo)

	baseURL := serveRepo(t, h, c.repo, extra)
	script := clientScript(baseURL, signed, "hello", "libfoo")
	out, err := runContainer(t, rt, image, script)
	t.Logf("%s alma client output:\n%s", profile, out)
	if err != nil {
		t.Fatalf("AlmaLinux client failed to consume the %s repo: %v", profile, err)
	}
	for _, want := range []string{"Hello, world!", "CLIENT_OK"} {
		if !strings.Contains(out, want) {
			t.Errorf("client output missing %q", want)
		}
	}
}

// TestE2EClientUpdate validates that a client sees an incrementally-updated
// repository: after publishing a newer package version with --prune-older, a
// fresh client installs the new version and the old one is gone. Runs on the
// local backend (the client read path is backend-independent — it is always
// HTTP) for each AlmaLinux version.
func TestE2EClientUpdate(t *testing.T) {
	if !gRPMs.haveNewer() {
		t.Skip("rpmbuild unavailable; no newer hello build to update to")
	}
	rt := containerRuntime(t)
	h := &localHarness{}
	t.Run(h.Name(), func(t *testing.T) {
		h.Start(t)
		for _, ap := range almaProfiles {
			t.Run("alma"+ap.ver, func(t *testing.T) {
				image := "almalinux:" + ap.ver
				ensureImage(t, rt, image)

				c := newTctx(t, h)
				// Publish 2.10-3, then supersede it with 2.10-4 (pruned blob is
				// garbage-collected automatically).
				c.addWith([]string{"--target", ap.profile}, gRPMs.hello)
				c.addWith([]string{"--target", ap.profile, "--prune-older"},
					gRPMs.helloNewer)

				baseURL := serveRepo(t, h, c.repo, nil)
				script := clientScript(baseURL, false, "hello")
				out, err := runContainer(t, rt, image, script)
				t.Logf("%s update client output:\n%s", ap.profile, out)
				if err != nil {
					t.Fatalf("client failed against updated %s repo: %v", ap.profile, err)
				}
				if !strings.Contains(out, "hello-2.10-4") {
					t.Errorf("client did not install the updated version 2.10-4:\n%s", out)
				}
				if strings.Contains(out, "hello-2.10-3") {
					t.Errorf("client still saw the superseded version 2.10-3:\n%s", out)
				}
			})
		}
	})
}

// clientScript builds the bash program run inside the AlmaLinux container. It
// configures a single repo pointing at baseURL, disabling all other repos so
// the transaction is satisfied entirely from our repository plus packages
// already present in the base image (no external mirrors needed).
func clientScript(baseURL string, signed bool, pkgs ...string) string {
	gpgcheck, repoGPGCheck, keyLine, importLine := "0", "0", "", ""
	if signed {
		gpgcheck, repoGPGCheck = "1", "1"
		keyLine = "gpgkey=" + baseURL + clientKeyFile
		importLine = "rpm --import " + baseURL + clientKeyFile
	}
	dnf := "dnf --disablerepo='*' --enablerepo=e2e -y"
	return fmt.Sprintf(`set -euo pipefail
rpm --version
%s
cat >/etc/yum.repos.d/e2e.repo <<EOF
[e2e]
name=e2e test repository
baseurl=%s
enabled=1
gpgcheck=%s
repo_gpgcheck=%s
%s
EOF
%s makecache
%s install %s
hello
rpm -q %s
echo CLIENT_OK
`, importLine, baseURL, gpgcheck, repoGPGCheck, keyLine,
		dnf, dnf, strings.Join(pkgs, " "), strings.Join(pkgs, " "))
}

// serveRepo starts an HTTP server (bound to loopback) that fronts the
// repository on backend h, plus any extra virtual files (e.g. the GPG key).
// Container clients reach it via --network=host at 127.0.0.1. Returns the base
// URL with a trailing slash.
func serveRepo(t *testing.T, h Harness, repoURL string, extra map[string][]byte) string {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/")
		if data, ok := extra[rel]; ok {
			http.ServeContent(w, r, rel, time.Time{}, bytes.NewReader(data))
			return
		}
		data, found, err := h.Fetch(repoURL, rel)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, rel, time.Time{}, bytes.NewReader(data))
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL + "/"
}

// runContainer runs image with host networking and the given bash script.
func runContainer(t *testing.T, rt, image, script string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, rt, "run", "--rm", "--network=host",
		image, "bash", "-c", script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// --- container runtime selection & image management -------------------------

// containerRuntime picks docker or podman, honoring CR_E2E_RUNTIME, and skips
// the test when neither is usable.
func containerRuntime(t *testing.T) string {
	t.Helper()
	if rt := os.Getenv("CR_E2E_RUNTIME"); rt != "" {
		return rt
	}
	for _, rt := range []string{"docker", "podman"} {
		if _, ok := haveExec(rt); ok {
			return rt
		}
	}
	t.Skip("neither docker nor podman is available; skipping AlmaLinux client tests")
	return ""
}

var (
	imgMu    sync.Mutex
	imgKnown = map[string]error{}
)

// ensureImage makes sure image is present locally, pulling it once if needed,
// and skips the test if it cannot be obtained (e.g. no registry access).
func ensureImage(t *testing.T, rt, image string) {
	t.Helper()
	imgMu.Lock()
	defer imgMu.Unlock()
	if err, ok := imgKnown[image]; ok {
		if err != nil {
			t.Skipf("image %s unavailable: %v", image, err)
		}
		return
	}
	if exec.Command(rt, "image", "inspect", image).Run() == nil {
		imgKnown[image] = nil
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, rt, "pull", image).CombinedOutput()
	imgKnown[image] = err
	if err != nil {
		t.Skipf("could not pull %s: %v\n%s", image, err, out)
	}
}
