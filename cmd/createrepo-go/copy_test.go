package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danudey/createrepo-go/pkg/repo"
)

const (
	helloRPM  = "../../reference/rpmbuild/RPMS/noarch/hello-2.10-3.noarch.rpm"
	libfooRPM = "../../reference/rpmbuild/RPMS/x86_64/libfoo-1.3.0-1.x86_64.rpm"
)

// runCLI executes the root command with a fresh flag set, so the global flag
// state one case leaves behind cannot leak into the next.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := rootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

// makeRepo publishes the reference RPMs into dir. When base is non-empty every
// package's <location> is given that xml:base, which is how an upstream
// repository tells clients to fetch packages from somewhere else.
func makeRepo(t *testing.T, dir, base string) {
	t.Helper()
	r, err := repo.Open(context.Background(), dir, repo.Options{Create: true, LocationPrefix: "Packages"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, p := range []string{helloRPM, libfooRPM} {
		if _, err := r.AddRPM(p); err != nil {
			t.Fatal(err)
		}
	}
	if base != "" {
		for _, p := range r.Index().Packages() {
			p.XMLBase = base
		}
	}
	if _, err := r.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func packageCount(t *testing.T, dir string) int {
	t.Helper()
	r, err := repo.Open(context.Background(), dir, repo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	return r.Index().Len()
}

func TestCopyRefusesExistingDestination(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	makeRepo(t, src, "")

	if _, err := runCLI(t, "copy", src, dst); err != nil {
		t.Fatalf("first copy should succeed: %v", err)
	}
	out, err := runCLI(t, "copy", src, dst)
	if err == nil {
		t.Fatal("copying onto an existing repository should fail without a flag")
	}
	for _, want := range []string{"--overwrite", "--continue", "--update"} {
		if !strings.Contains(err.Error()+out, want) {
			t.Errorf("the error should point at %s: %v", want, err)
		}
	}

	if _, err := runCLI(t, "copy", src, dst, "--continue"); err != nil {
		t.Errorf("--continue should be accepted: %v", err)
	}
}

func TestCopyContinueRejectsADifferentRepository(t *testing.T) {
	src, other, dst := t.TempDir(), t.TempDir(), t.TempDir()
	makeRepo(t, src, "")
	makeRepo(t, other, "")

	if _, err := runCLI(t, "copy", src, dst); err != nil {
		t.Fatal(err)
	}
	_, err := runCLI(t, "copy", other, dst, "--continue")
	if err == nil {
		t.Fatal("--continue into a copy of another repository should fail")
	}
	if !strings.Contains(err.Error(), "copied from") {
		t.Errorf("error should explain the mismatch, got: %v", err)
	}
}

func TestCopyRefusesMetadataThatPinsPackagesElsewhere(t *testing.T) {
	const upstream = "https://upstream.example.com/el9/"
	src := t.TempDir()
	makeRepo(t, src, upstream)

	_, err := runCLI(t, "copy", src, t.TempDir())
	if err == nil {
		t.Fatal("an exact copy of metadata carrying xml:base should be refused")
	}
	if !strings.Contains(err.Error(), upstream) {
		t.Errorf("the error should name the upstream URL, got: %v", err)
	}

	// --force accepts the copy as it stands...
	forced := t.TempDir()
	if _, err := runCLI(t, "copy", src, forced, "--force"); err != nil {
		t.Fatalf("--force should permit the verbatim copy: %v", err)
	}
	if got := xmlBaseOf(t, forced); got != upstream {
		t.Errorf("a forced copy should keep xml:base, got %q", got)
	}

	// ...and --remove-baseurl fixes it instead.
	fixed := t.TempDir()
	if _, err := runCLI(t, "copy", src, fixed, "--remove-baseurl"); err != nil {
		t.Fatalf("--remove-baseurl should permit the copy: %v", err)
	}
	if got := xmlBaseOf(t, fixed); got != "" {
		t.Errorf("--remove-baseurl left xml:base = %q", got)
	}
	if n := packageCount(t, fixed); n != 2 {
		t.Errorf("the fixed copy holds %d packages, want 2", n)
	}
}

// xmlBaseOf returns the xml:base recorded for the first package of the
// repository at dir.
func xmlBaseOf(t *testing.T, dir string) string {
	t.Helper()
	r, err := repo.Open(context.Background(), dir, repo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	pkgs := r.Index().Packages()
	if len(pkgs) == 0 {
		t.Fatalf("%s holds no packages", dir)
	}
	return pkgs[0].XMLBase
}

func TestCopySelectionRebuildsMetadata(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	makeRepo(t, src, "")

	if _, err := runCLI(t, "copy", src, dst, "--include", "hello"); err != nil {
		t.Fatal(err)
	}
	if n := packageCount(t, dst); n != 1 {
		t.Errorf("--include hello copied %d packages, want 1", n)
	}
	if _, err := filepath.Glob(filepath.Join(dst, "Packages", "hello-*.rpm")); err != nil {
		t.Fatal(err)
	}
}

func TestCopyUpdateMergesIntoAnExistingRepository(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	makeRepo(t, src, "")

	if _, err := runCLI(t, "copy", src, dst, "--include", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, "copy", src, dst, "--update"); err != nil {
		t.Fatalf("--update should merge the rest in: %v", err)
	}
	if n := packageCount(t, dst); n != 2 {
		t.Errorf("after --update the destination holds %d packages, want 2", n)
	}
}

func TestCopyFlagValidation(t *testing.T) {
	cases := []struct {
		name string
		cf   copyFlags
	}{
		{"overwrite with continue", copyFlags{overwrite: true, resume: true}},
		{"continue with update", copyFlags{resume: true, update: true}},
		{"both baseurl options", copyFlags{baseURL: "https://x/", removeBaseURL: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.cf.validate(); err == nil {
				t.Error("expected the combination to be rejected")
			}
		})
	}
	if err := (&copyFlags{update: true}).validate(); err != nil {
		t.Errorf("a single destination-state flag is fine: %v", err)
	}
}

func TestNormalizeKeyID(t *testing.T) {
	const fpr = "FF652CAEF8C583AB827F219E992DF1A087059154"
	if got, want := normalizeKeyID(fpr), "992df1a087059154"; got != want {
		t.Errorf("normalizeKeyID(fingerprint) = %q, want the long key id %q", got, want)
	}
	if got := normalizeKeyID("992DF1A087059154"); got != "992df1a087059154" {
		t.Errorf("a long key id should only be lowercased, got %q", got)
	}
}
