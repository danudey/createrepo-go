package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A Valid-Until set with --validity must survive later publishes that do not
// repeat the flag. Before, the first add without it wrote a Release that never
// expires, with no warning.
func TestValidityIsKeptAcrossPublishes(t *testing.T) {
	deb, err := filepath.Abs(filepath.Join("..", "..", "reference", "apt", "packages", "hello_2.10-3_all.deb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(deb); err != nil {
		t.Skipf("reference packages are not built; run reference/apt/gen.sh (%v)", err)
	}
	run := func(args ...string) string {
		t.Helper()
		root := rootCmd()
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetErr(&buf)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, buf.String())
		}
		return buf.String()
	}
	dir := t.TempDir()
	release := func() string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "dists", "stable", "Release"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	run("create", dir, "--validity", "168h", "--no-sign-release")
	if !strings.Contains(release(), "Valid-Until:") {
		t.Fatal("create --validity wrote no Valid-Until")
	}
	run("add", dir, deb, "--no-sign-release")
	if !strings.Contains(release(), "Valid-Until:") {
		t.Error("an add without --validity dropped the recorded Valid-Until")
	}
	// An explicit --validity 0 still turns expiry off, and is remembered.
	run("add", dir, deb, "--validity", "0", "--no-sign-release")
	run("add", dir, deb, "--no-sign-release")
	if strings.Contains(release(), "Valid-Until:") {
		t.Error("--validity 0 did not turn Valid-Until off for later publishes")
	}
}
