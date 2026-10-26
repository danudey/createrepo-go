package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func referenceDeb(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "reference", "apt", "packages", name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("reference packages are not built; run reference/apt/gen.sh (%v)", err)
	}
	return p
}

// A transforming copy into a destination whose config records another suite
// must publish into the suite it checked and reported. Before, the recorded
// suite replaced it after the checks, so the packages were merged into that
// suite instead, past the --overwrite/--update guards.
func TestCopyPublishesIntoTheSuiteItReports(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	if out, err := runCopyCLI(t, "add", src, referenceDeb(t, "hello_2.10-3_all.deb"), "--suite", "bookworm", "--no-sign-release"); err != nil {
		t.Fatalf("add to source: %v\n%s", err, out)
	}
	if out, err := runCopyCLI(t, "create", dst, "--suite", "stable", "--no-sign-release"); err != nil {
		t.Fatalf("create destination: %v\n%s", err, out)
	}

	out, err := runCopyCLI(t, "copy", src, dst, "--latest-only", "--no-sign-release")
	if err != nil {
		t.Fatalf("copy: %v\n%s", err, out)
	}
	if !strings.Contains(out, "bookworm") {
		t.Fatalf("the copy did not report the bookworm suite:\n%s", out)
	}
	packages, err := os.ReadFile(filepath.Join(dst, "dists", "bookworm", "main", "binary-all", "Packages"))
	if err != nil || !strings.Contains(string(packages), "Package: hello") {
		t.Errorf("hello was not published into bookworm (%v):\n%s", err, packages)
	}
	if stable, _ := os.ReadFile(filepath.Join(dst, "dists", "stable", "main", "binary-all", "Packages")); strings.Contains(string(stable), "Package: hello") {
		t.Error("hello was published into the destination's recorded suite, stable, instead")
	}
}

// runCopyCLI executes the root command with a fresh flag set and returns its
// combined output.
func runCopyCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := rootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}
