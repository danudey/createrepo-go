package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// republish changes a package file in place, leaving it a parseable RPM. It
// stands in for a package rebuilt and republished under the same name, which is
// what leaves a repository's metadata describing content that is no longer
// there.
func republish(t *testing.T, dir, name string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "Packages", name+"*.rpm"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected one %s package in %s, found %v (%v)", name, dir, matches, err)
	}
	f, err := os.OpenFile(matches[0], os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRebuildRegeneratesMetadataFromLocalPackages(t *testing.T) {
	dir := t.TempDir()
	makeRepo(t, dir, "")
	republish(t, dir, "hello")

	if _, err := runCLI(t, "verify", dir); err == nil {
		t.Fatal("verify should fail while the metadata describes the old file")
	}

	out, err := runCLI(t, "rebuild", dir, "--yes")
	if err != nil {
		t.Fatalf("rebuild: %v\n%s", err, out)
	}
	if !strings.Contains(out, "re-read from RPMs") {
		t.Errorf("rebuild did not report re-reading the packages:\n%s", out)
	}

	if _, err := runCLI(t, "verify", dir); err != nil {
		t.Errorf("verify should pass after rebuild re-read the packages: %v", err)
	}
}

func TestRebuildWarnsWhenItDoesNotReadThePackages(t *testing.T) {
	dir := t.TempDir()
	makeRepo(t, dir, "")
	republish(t, dir, "hello")

	out, err := runCLI(t, "rebuild", dir, "--yes", "--from-packages=false")
	if err != nil {
		t.Fatalf("rebuild: %v\n%s", err, out)
	}
	if !strings.Contains(out, "not re-read from the RPM files") {
		t.Errorf("rebuild did not warn that it was publishing unchecked metadata:\n%s", out)
	}
	if !strings.Contains(out, "--from-packages") {
		t.Errorf("the warning should name the flag that fixes it:\n%s", out)
	}

	// And the metadata is indeed still wrong, which is what the warning said.
	if _, err := runCLI(t, "verify", dir); err == nil {
		t.Error("verify should still fail when the packages were not re-read")
	}
}
