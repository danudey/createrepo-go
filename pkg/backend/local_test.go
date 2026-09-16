package backend

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestLocalList(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"repodata/repomd.xml":      "<repomd/>",
		"repodata/abc-primary.xml": "primary",
		"Packages/hello.rpm":       "hello",
		"Packages/sub/libfoo.rpm":  "libfoo",
		"createrepo-go.json":       "{}",
	}
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	be, err := newLocal(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("whole repo", func(t *testing.T) {
		objs, err := be.List(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		got := paths(objs)
		want := []string{
			"Packages/hello.rpm", "Packages/sub/libfoo.rpm",
			"createrepo-go.json", "repodata/abc-primary.xml", "repodata/repomd.xml",
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("List(\"\") = %v; want %v", got, want)
		}
		// Sizes must be reported.
		for _, o := range objs {
			if o.Size == 0 {
				t.Errorf("object %s has zero size", o.Path)
			}
		}
	})

	t.Run("scoped to repodata", func(t *testing.T) {
		objs, err := be.List(context.Background(), "repodata/")
		if err != nil {
			t.Fatal(err)
		}
		got := paths(objs)
		want := []string{"repodata/abc-primary.xml", "repodata/repomd.xml"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("List(\"repodata/\") = %v; want %v", got, want)
		}
	})

	t.Run("missing prefix is empty", func(t *testing.T) {
		objs, err := be.List(context.Background(), "does-not-exist/")
		if err != nil {
			t.Fatalf("List of missing prefix errored: %v", err)
		}
		if len(objs) != 0 {
			t.Errorf("List of missing prefix = %v; want empty", paths(objs))
		}
	})
}

func paths(objs []ObjectInfo) []string {
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = o.Path
	}
	sort.Strings(out)
	return out
}
