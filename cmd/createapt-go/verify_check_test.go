package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unsignedSuite publishes debs into a new unsigned bookworm suite, with the
// given extra flags (--compression, --validity, …).
func unsignedSuite(t *testing.T, debs []string, extra ...string) string {
	t.Helper()
	dir := t.TempDir()
	args := []string{"add", dir}
	for _, d := range debs {
		args = append(args, referenceDeb(t, d))
	}
	args = append(args, append([]string{"--suite", "bookworm", "--no-sign-release"}, extra...)...)
	if out, err := runApt(t, args...); err != nil {
		t.Fatalf("publish: %v\n%s", err, out)
	}
	return dir
}

func TestVerifyPassesAHealthySuite(t *testing.T) {
	dir := unsignedSuite(t, []string{"hello_2.10-3_all.deb", "libfoo_1.3.0-1_amd64.deb"})
	for _, args := range [][]string{nil, {"--metadata-only"}, {"--checksums"}, {"--skip-package-contents"}, {"-v"}} {
		out, err := runApt(t, append([]string{"verify", dir}, args...)...)
		if err != nil {
			t.Errorf("verify %v: %v\n%s", args, err, out)
		}
	}
	out, _ := runApt(t, "verify", dir, "-v")
	for _, want := range []string{"ok   dists/bookworm/", "ok   hello_2.10-3_all", "control file matches"} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose output lacks %q:\n%s", want, out)
		}
	}
}

// The failure check existed for: one .deb published over another without
// regenerating the indexes.
func TestVerifyDetectsAReplacedPackage(t *testing.T) {
	dir := unsignedSuite(t, []string{"hello_2.10-3_all.deb"})
	other, err := os.ReadFile(referenceDeb(t, "libfoo_1.3.0-1_amd64.deb"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "pool", "main", "h", "hello", "hello_2.10-3_all.deb"), other, 0o644)

	out, err := runApt(t, "verify", dir)
	if err == nil || !strings.Contains(out, "size") || !strings.Contains(out, "its control file says Package: libfoo, but the index says hello") {
		t.Errorf("a replaced package: %v\n%s", err, out)
	}
	if _, err := runApt(t, "verify", dir, "--metadata-only"); err != nil {
		t.Errorf("--metadata-only looked at the packages: %v", err)
	}
}

func TestVerifyDetectsANonDebianFile(t *testing.T) {
	dir := unsignedSuite(t, []string{"hello_2.10-3_all.deb"})
	p := filepath.Join(dir, "pool", "main", "h", "hello", "hello_2.10-3_all.deb")
	fi, _ := os.Stat(p)
	os.WriteFile(p, []byte(strings.Repeat("x", int(fi.Size()))), 0o644)
	out, err := runApt(t, "verify", dir)
	if err == nil || !strings.Contains(out, "not a readable Debian package") {
		t.Errorf("a file of the right size that is not a .deb: %v\n%s", err, out)
	}
}

func TestVerifyDetectsACorruptedIndex(t *testing.T) {
	dir := unsignedSuite(t, []string{"hello_2.10-3_all.deb"}, "--compression", "gzip")
	matches, _ := filepath.Glob(filepath.Join(dir, "dists", "bookworm", "main", "binary-*", "Packages*"))
	if len(matches) == 0 {
		t.Fatal("no Packages index published")
	}
	for _, m := range matches {
		os.WriteFile(m, []byte("Package: tampered\n"), 0o644)
	}
	out, err := runApt(t, "verify", dir, "--metadata-only")
	if err == nil || !strings.Contains(err.Error(), "indexes cannot be read") || !strings.Contains(out, "does not match") {
		t.Errorf("an index that no longer matches the Release: %v\n%s", err, out)
	}
}

// apt fetches one compressed form of an index; another form the Release
// lists but the mirror does not publish (as Debian's own archive does with
// the uncompressed Packages) is not a defect.
func TestVerifyAcceptsAnUnpublishedIndexFormWhenAnotherIsSound(t *testing.T) {
	dir := unsignedSuite(t, []string{"hello_2.10-3_all.deb"}, "--compression", "gzip,xz", "--by-hash=false")
	forms, _ := filepath.Glob(filepath.Join(dir, "dists", "bookworm", "main", "binary-all", "Packages*"))
	if len(forms) < 2 {
		t.Fatalf("want several compressed forms, got %v", forms)
	}
	var removed string
	for _, f := range forms {
		if strings.HasSuffix(f, ".xz") {
			removed = f
			os.Remove(f)
		}
	}
	if removed == "" {
		t.Fatalf("no .xz form among %v", forms)
	}
	if out, err := runApt(t, "verify", dir, "--metadata-only"); err != nil {
		t.Errorf("one missing form with another sound: %v\n%s", err, out)
	}
	for _, f := range forms {
		os.Remove(f)
	}
	if out, err := runApt(t, "verify", dir, "--metadata-only"); err == nil {
		t.Errorf("every form of an index missing was accepted:\n%s", out)
	}
}

// With Acquire-By-Hash apt fetches each index by its checksum, so that copy
// has to be there, and a missing plain copy is recovered from it.
func TestVerifyChecksByHashCopies(t *testing.T) {
	dir := unsignedSuite(t, []string{"hello_2.10-3_all.deb"}, "--by-hash")
	plain, _ := filepath.Glob(filepath.Join(dir, "dists", "bookworm", "main", "binary-all", "Packages.gz"))
	if len(plain) != 1 {
		t.Fatalf("no Packages.gz: %v", plain)
	}
	os.Remove(plain[0])
	if out, err := runApt(t, "verify", dir, "--metadata-only"); err != nil {
		t.Errorf("a plain index missing with its by-hash copy present: %v\n%s", err, out)
	}
	os.RemoveAll(filepath.Join(dir, "dists", "bookworm", "main", "binary-all", "by-hash"))
	out, err := runApt(t, "verify", dir, "--metadata-only")
	if err == nil || !strings.Contains(out, "by-hash") {
		t.Errorf("by-hash copies missing: %v\n%s", err, out)
	}
}

func TestVerifyReportsAnExpiredRelease(t *testing.T) {
	dir := unsignedSuite(t, []string{"hello_2.10-3_all.deb"}, "--validity", "1s")
	time.Sleep(1500 * time.Millisecond)
	out, err := runApt(t, "verify", dir)
	if err == nil || !strings.Contains(out, "Valid-Until") {
		t.Errorf("an expired Release: %v\n%s", err, out)
	}
}

func TestVerifySuites(t *testing.T) {
	dir := unsignedSuite(t, []string{"hello_2.10-3_all.deb"})
	if out, err := runApt(t, "verify", dir); err != nil {
		t.Errorf("verify with the recorded suite: %v\n%s", err, out)
	}
	out, err := runApt(t, "verify", dir, "--suite", "trixie")
	if err == nil || !strings.Contains(out+err.Error(), "neither InRelease nor Release") {
		t.Errorf("a missing suite: %v\n%s", err, out)
	}
	out, err = runApt(t, "verify", dir, "--suite", "bookworm,trixie")
	if err == nil || !strings.Contains(err.Error(), "1 of 2") || !strings.Contains(out, "[bookworm] OK") {
		t.Errorf("two suites, one missing: %v\n%s", err, out)
	}
}

func TestVerifySelectsPackages(t *testing.T) {
	dir := unsignedSuite(t, []string{"hello_2.10-3_all.deb", "libfoo_1.3.0-1_amd64.deb"})
	removed := false
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), "libfoo_") && strings.HasSuffix(p, ".deb") {
			removed = os.Remove(p) == nil
		}
		return nil
	})
	if !removed {
		t.Fatal("libfoo was not published")
	}
	for _, tc := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"--packages", "hello"}, true},
		{[]string{"--arch", "arm64"}, true}, // only the all package applies
		{[]string{"--arch", "amd64"}, false},
		{nil, false},
	} {
		out, err := runApt(t, append([]string{"verify", dir}, tc.args...)...)
		if (err == nil) != tc.ok {
			t.Errorf("verify %v: err = %v, want ok=%v\n%s", tc.args, err, tc.ok, out)
		}
	}
}

func TestVerifyFromASourcesFile(t *testing.T) {
	dir := unsignedSuite(t, []string{"hello_2.10-3_all.deb"})
	list := filepath.Join(t.TempDir(), "x.list")
	os.WriteFile(list, []byte("deb [trusted=yes] file://"+dir+" bookworm main\n"), 0o600)
	if out, err := runApt(t, "verify", list); err != nil || !strings.Contains(out, "OK") {
		t.Errorf("verify from a sources file: %v\n%s", err, out)
	}
}

// A sources file someone else published may only name http(s) repositories:
// it must not make verify read local files or use the operator's credentials.
func TestVerifyRemoteSourcesFileCannotNameLocalPaths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("deb file:///etc bookworm main\n"))
	}))
	defer srv.Close()
	if out, err := runApt(t, "verify", srv.URL+"/x.list"); err == nil {
		t.Errorf("a downloaded sources file named a local path:\n%s", out)
	}
}
