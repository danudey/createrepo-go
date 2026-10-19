package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

// An object path comes from a repository's metadata, so a hostile one can name
// anything. None of them may reach a file outside the root.
func TestLocalRefusesPathsOutsideTheRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	be, err := newLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, rel := range []string{"../escaped", "pool/../../escaped", "/../escaped", "..", `..\escaped`} {
		if filepath.Separator != '\\' && strings.Contains(rel, `\`) {
			continue // a backslash is an ordinary filename character here
		}
		if err := be.Put(ctx, rel, strings.NewReader("x"), 1); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("Put(%q) = %v, want ErrUnsafePath", rel, err)
		}
		if _, err := be.Get(ctx, rel); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("Get(%q) = %v, want ErrUnsafePath", rel, err)
		}
		if err := be.Delete(ctx, rel); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("Delete(%q) = %v, want ErrUnsafePath", rel, err)
		}
		if err := be.Copy(ctx, "a", rel); !errors.Is(err, ErrUnsafePath) && !errors.Is(err, ErrNotExist) {
			t.Errorf("Copy(a, %q) = %v, want an error", rel, err)
		}
		if p, ok := LocalPath(be, rel); ok {
			t.Errorf("LocalPath(%q) = %q, want none", rel, p)
		}
	}
	if _, err := os.Stat(filepath.Join(parent, "escaped")); !os.IsNotExist(err) {
		t.Errorf("a file was written outside the root: %v", err)
	}

	// Paths that only look odd still resolve inside the root.
	for _, rel := range []string{"/pool/a.deb", "pool/../dists/Release", "./x"} {
		if err := be.Put(ctx, rel, strings.NewReader("x"), 1); err != nil {
			t.Errorf("Put(%q) = %v, want success", rel, err)
		}
	}
}

func TestRemoteRel(t *testing.T) {
	for _, rel := range []string{"..", "../x", "a/../../x", "/../x", "//../x"} {
		if _, err := remoteRel(rel); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("remoteRel(%q) = %v, want ErrUnsafePath", rel, err)
		}
	}
	for rel, want := range map[string]string{"": "", "/a/b": "a/b", "a/../b": "a/../b", "..a/b": "..a/b"} {
		if got, err := remoteRel(rel); err != nil || got != want {
			t.Errorf("remoteRel(%q) = %q, %v, want %q", rel, got, err, want)
		}
	}
}

// Published files must be readable by the web server that serves the
// repository, whoever ran the publish. os.CreateTemp makes 0600 files, and
// both Put and Copy rename one into place.
func TestLocalPublishesWorldReadableFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	ctx := context.Background()
	be, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := be.Put(ctx, "repodata/repomd.xml", strings.NewReader("<repomd/>"), -1); err != nil {
		t.Fatal(err)
	}
	if err := be.(Copier).Copy(ctx, "repodata/repomd.xml", "Packages/copy.xml"); err != nil {
		t.Fatal(err)
	}
	root := be.String()
	for _, rel := range []string{"repodata/repomd.xml", "Packages/copy.xml"} {
		fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if mode := fi.Mode().Perm(); mode&0o044 != 0o044 {
			t.Errorf("%s is %v; a web server running as another user could not read it", rel, mode)
		}
	}
}
