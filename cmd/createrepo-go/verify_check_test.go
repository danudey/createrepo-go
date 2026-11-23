package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danudey/createrepo-go/pkg/rpm/repodata"
)

// twoPackageRepo publishes hello (noarch) and libfoo (x86_64) unsigned.
func twoPackageRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := runCLI(t, "add", dir, helloRPM, libfooRPM); err != nil {
		t.Fatalf("publish: %v\n%s", err, out)
	}
	return dir
}

func repomdOf(t *testing.T, dir string) *repodata.Repomd {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "repodata", "repomd.xml"))
	if err != nil {
		t.Fatal(err)
	}
	md, err := repodata.ParseRepomd(data)
	if err != nil {
		t.Fatal(err)
	}
	return md
}

// Metadata too broken to load is reported as problems, not as a bare load
// error, so the user sees every defect.
func TestVerifyReportsUnloadableMetadataAsProblems(t *testing.T) {
	dir := twoPackageRepo(t)
	primary := filepath.Join(dir, repomdOf(t, dir).DataEntryByType("primary").Location)
	data, _ := os.ReadFile(primary)
	data[len(data)/2] ^= 0xff
	os.WriteFile(primary, data, 0o644)

	out, err := runCLI(t, "verify", dir)
	if err == nil || !strings.Contains(err.Error(), "package index cannot be read") {
		t.Fatalf("verify = %v\n%s", err, out)
	}
	if !strings.Contains(out, "PROBLEM: primary metadata: sha256 checksum") {
		t.Errorf("the primary checksum mismatch was not reported:\n%s", out)
	}
}

func TestVerifyMetadataOnly(t *testing.T) {
	dir := twoPackageRepo(t)
	os.RemoveAll(filepath.Join(dir, "Packages"))

	out, err := runCLI(t, "verify", dir, "--metadata-only")
	if err != nil {
		t.Fatalf("--metadata-only looked at the packages: %v\n%s", err, out)
	}
	if !strings.Contains(out, "metadata: 3 document(s) intact") || !strings.Contains(out, "2 package(s) indexed, not checked") {
		t.Errorf("summary:\n%s", out)
	}
	if _, err := runCLI(t, "verify", dir); err == nil {
		t.Error("verify without --metadata-only passed with every package missing")
	}
}

func TestVerifySelectsPackages(t *testing.T) {
	dir := twoPackageRepo(t)
	matches, _ := filepath.Glob(filepath.Join(dir, "Packages", "libfoo-*.rpm"))
	if len(matches) != 1 {
		t.Fatalf("libfoo not published where expected: %v", matches)
	}
	os.Remove(matches[0])

	for _, tc := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"--packages", "hello"}, true},
		{[]string{"--arch", "aarch64"}, true}, // only noarch hello applies
		{[]string{"--arch", "x86_64"}, false},
		{[]string{"--packages", "hello,libfoo"}, false},
		{[]string{"--versions", "latest"}, false},
	} {
		out, err := runCLI(t, append([]string{"verify", dir}, tc.args...)...)
		if (err == nil) != tc.ok {
			t.Errorf("verify %v: err = %v, want ok=%v\n%s", tc.args, err, tc.ok, out)
		}
	}
	if _, err := runCLI(t, "verify", dir, "--versions", "some"); err == nil {
		t.Error("an invalid --versions was accepted")
	}
}

func TestVerifyVerbose(t *testing.T) {
	dir := twoPackageRepo(t)
	out, err := runCLI(t, "verify", dir, "-v")
	if err != nil {
		t.Fatalf("verify -v: %v\n%s", err, out)
	}
	for _, want := range []string{"ok   repodata/", "(primary)", "ok   hello-2.10-3.noarch", "ok   libfoo-1.3.0-1.x86_64"} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose output lacks %q:\n%s", want, out)
		}
	}
}

// One .repo section with $releasever describes a repository per release;
// --releasever 8,9 verifies each.
func TestVerifyRepoFileReleaseMatrix(t *testing.T) {
	isolateGPG(t)
	root := t.TempDir()
	for _, rel := range []string{"8", "9"} {
		if out, err := runCLI(t, "add", filepath.Join(root, "el"+rel), helloRPM); err != nil {
			t.Fatalf("publish el%s: %v\n%s", rel, err, out)
		}
	}
	repoFile := filepath.Join(t.TempDir(), "x.repo")
	os.WriteFile(repoFile, []byte("[x]\nbaseurl=file://"+root+"/el$releasever\n"), 0o600)

	out, err := runCLI(t, "verify", repoFile, "--releasever", "8,9")
	if err != nil {
		t.Fatalf("verify both releases: %v\n%s", err, out)
	}
	for _, want := range []string{"[x releasever=8] OK", "[x releasever=9] OK"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	os.RemoveAll(filepath.Join(root, "el9", "Packages"))
	out, err = runCLI(t, "verify", repoFile, "--releasever", "8,9")
	if err == nil || !strings.Contains(err.Error(), "1 of 2") || !strings.Contains(out, "[x releasever=9] PROBLEM") {
		t.Errorf("a broken el9 was not reported on its own: %v\n%s", err, out)
	}
}
