package xfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danudey/createrepo-go/pkg/backend"
)

func TestTempSuffix(t *testing.T) {
	for in, want := range map[string]string{
		"pool/main/h/hello_1.0_all.deb": "hello_1.0_all.deb",
		"Packages/x.rpm":                "x.rpm",
		`evil\..\..\name.rpm`:           "name.rpm",
		"..":                            "obj",
		"":                              "obj",
		"dir/":                          "dir",
	} {
		if got := TempSuffix(in); got != want {
			t.Errorf("TempSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

// newRepo returns a local backend holding files.
func newRepo(t *testing.T, files map[string]string) backend.Backend {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	be, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return be
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestFetchToTempVerifiesSizeAndChecksum(t *testing.T) {
	be := newRepo(t, map[string]string{"pool/a.deb": "package"})
	ctx := context.Background()

	local, n, err := FetchToTemp(ctx, be, Object{Path: "pool/a.deb", Size: 7, Checksum: sum("package")}, "test", nil)
	if err != nil {
		t.Fatalf("matching object: %v", err)
	}
	defer os.Remove(local)
	if n != 7 {
		t.Errorf("size = %d, want 7", n)
	}

	if _, _, err := FetchToTemp(ctx, be, Object{Path: "pool/a.deb", Size: 8}, "test", nil); err == nil {
		t.Error("a size mismatch was accepted")
	}
	if _, _, err := FetchToTemp(ctx, be, Object{Path: "pool/a.deb", Size: -1, Checksum: sum("other")}, "test", nil); err == nil {
		t.Error("a checksum mismatch was accepted")
	}
}

func TestDestinationMatches(t *testing.T) {
	dst := newRepo(t, map[string]string{"pool/a.deb": "package", "Release": "v1"})
	ctx := context.Background()
	for _, tc := range []struct {
		obj  Object
		want bool
	}{
		{Object{Path: "pool/a.deb", Size: 7, Checksum: sum("package")}, true},
		{Object{Path: "pool/a.deb", Size: 7, Checksum: sum("pACKAGE")}, false},
		{Object{Path: "pool/a.deb", Size: 7}, true},
		{Object{Path: "pool/a.deb", Size: 8}, false},
		{Object{Path: "Release", Size: 2, Volatile: true}, false},
		{Object{Path: "missing", Size: 2}, false},
	} {
		got, err := DestinationMatches(ctx, dst, tc.obj)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("DestinationMatches(%+v) = %v, want %v", tc.obj, got, tc.want)
		}
	}
}

func TestCopyExactIsResumableAndPrunes(t *testing.T) {
	files := map[string]string{"pool/a.deb": "aaa", "pool/b.deb": "bbbb", "Release": "rel"}
	src := newRepo(t, files)
	dst := newRepo(t, map[string]string{"pool/a.deb": "aaa", "stray": "x"})
	objs := []Object{
		{Path: "Release", Size: 3, Volatile: true},
		{Path: "pool/a.deb", Size: 3, Checksum: sum("aaa")},
		{Path: "pool/b.deb", Size: 4, Checksum: sum("bbbb")},
	}
	Sort(objs, func(o Object) int {
		if o.Path == "Release" {
			return 1
		}
		return 0
	})
	if objs[len(objs)-1].Path != "Release" {
		t.Fatalf("Release not last: %v", objs)
	}

	var order []string
	stats, err := CopyExact(context.Background(), src, dst, objs, CopyOptions{
		Prune:    true,
		Progress: func(action string, o Object) { order = append(order, action+" "+o.Path) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 2 || stats.Skipped != 1 || len(stats.Deleted) != 1 || stats.Deleted[0] != "stray" {
		t.Errorf("stats = %+v, want 2 copied, 1 skipped, stray deleted", stats)
	}
	want := "skip pool/a.deb,copy pool/b.deb,copy Release,delete stray"
	if got := strings.Join(order, ","); got != want {
		t.Errorf("actions = %s, want %s", got, want)
	}
	for rel, body := range files {
		got, err := ReadAll(context.Background(), dst, rel)
		if err != nil || string(got) != body {
			t.Errorf("%s at the destination = %q, %v; want %q", rel, got, err, body)
		}
	}
}

// A hostile source repository can name any path. The copy refuses it before
// anything is transferred.
func TestCopyExactRefusesUnsafePathsUpFront(t *testing.T) {
	src := newRepo(t, map[string]string{"pool/a.deb": "aaa"})
	dst := newRepo(t, nil)
	objs := []Object{
		{Path: "pool/a.deb", Size: 3},
		{Path: "../escaped", Size: 3},
	}
	_, err := CopyExact(context.Background(), src, dst, objs, CopyOptions{})
	if !errors.Is(err, backend.ErrUnsafePath) {
		t.Fatalf("CopyExact = %v, want ErrUnsafePath", err)
	}
	if _, err := dst.Stat(context.Background(), "pool/a.deb"); !errors.Is(err, backend.ErrNotExist) {
		t.Errorf("a file was copied before the unsafe path was refused (stat: %v)", err)
	}
}

func TestLocalCopyReadsLocalFilesInPlace(t *testing.T) {
	be := newRepo(t, map[string]string{"pool/a.deb": "aaa"})
	local, cleanup, n, err := LocalCopy(context.Background(), be, "pool/a.deb", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if n != 0 {
		t.Errorf("transferred = %d for a local file, want 0", n)
	}
	if want, _ := backend.LocalPath(be, "pool/a.deb"); local != want {
		t.Errorf("LocalCopy = %q, want the file in place (%q)", local, want)
	}
	if _, _, _, err := LocalCopy(context.Background(), be, "missing", "test", nil); !errors.Is(err, backend.ErrNotExist) {
		t.Errorf("missing file: err = %v, want ErrNotExist", err)
	}
}

func TestHashObject(t *testing.T) {
	be := newRepo(t, map[string]string{"a": "hello"})
	got, n, err := HashObject(context.Background(), be, "a", nil)
	if err != nil || got != sum("hello") || n != 5 {
		t.Errorf("HashObject = %s, %d, %v; want %s, 5", got, n, err, sum("hello"))
	}
}
