package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandRPMArgs(t *testing.T) {
	dir := t.TempDir()
	// A nested tree with .rpm files, a .RPM (uppercase), and a non-rpm file.
	mk := func(rel string) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := mk("a.rpm")
	sub := mk("sub/b.rpm")
	deep := mk("sub/deeper/c.RPM")
	mk("sub/notes.txt")

	t.Run("recursive directory scan", func(t *testing.T) {
		got, err := expandRPMArgs([]string{dir})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{a, sub, deep} // sorted: a.rpm, sub/b.rpm, sub/deeper/c.RPM
		if len(got) != len(want) {
			t.Fatalf("got %d files %v, want %d %v", len(got), got, len(want), want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("dedupes file listed alongside its directory", func(t *testing.T) {
		got, err := expandRPMArgs([]string{a, dir})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Errorf("expected 3 unique files, got %d: %v", len(got), got)
		}
	})

	t.Run("explicit non-rpm file is kept", func(t *testing.T) {
		txt := filepath.Join(dir, "sub", "notes.txt")
		got, err := expandRPMArgs([]string{txt})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != txt {
			t.Errorf("explicit file not kept: %v", got)
		}
	})

	t.Run("missing path errors", func(t *testing.T) {
		if _, err := expandRPMArgs([]string{filepath.Join(dir, "nope")}); err == nil {
			t.Errorf("expected error for missing path")
		}
	})

	t.Run("empty directory errors", func(t *testing.T) {
		empty := t.TempDir()
		if _, err := expandRPMArgs([]string{empty}); err == nil {
			t.Errorf("expected error when no .rpm files are found")
		}
	})
}
