package repo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/rpm/repodata"
	"github.com/danudey/createrepo-go/pkg/rpm/rpmmeta"
)

// copyTo writes the RPM at src to dst, creating dst's directory.
func copyTo(t *testing.T, src, dst string) string {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dst
}

// Two different packages whose files share a name would be indexed at one
// location, where the repository can hold only one file; clients installing
// the other would fail its checksum.
func TestAddRefusesTwoPackagesAtOneLocation(t *testing.T) {
	r, _ := newLocalRepo(t, t.TempDir(), true)
	defer r.Close()
	if _, err := r.AddRPM(helloRPM()); err != nil {
		t.Fatal(err)
	}
	misnamed := copyTo(t, libfooRPM(), filepath.Join(t.TempDir(), filepath.Base(helloRPM())))
	_, err := r.AddRPM(misnamed)
	if err == nil || !strings.Contains(err.Error(), "would both be published at") {
		t.Fatalf("AddRPM of a different package under the same file name = %v, want a clash", err)
	}
	if r.Index().Len() != 1 {
		t.Errorf("the refused package was indexed anyway (%d packages)", r.Index().Len())
	}
}

// A relocation that flattens two directories into one can put two packages at
// one location. Commit must refuse before anything moves.
func TestCommitRefusesARelocationThatCollides(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	inner, err := backend.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Publish hello at a/pkg.rpm and libfoo at b/pkg.rpm: distinct locations.
	staging := t.TempDir()
	for _, step := range []struct{ prefix, rpm string }{{"a", helloRPM()}, {"b", libfooRPM()}} {
		r, err := OpenWith(ctx, inner, Options{Create: true, LocationPrefix: step.prefix, now: 1000})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.AddRPM(copyTo(t, step.rpm, filepath.Join(staging, step.prefix, "pkg.rpm"))); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	cb := wrap(inner)
	r, err := OpenWith(ctx, cb, Options{now: 1000})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.RelocateAll("")
	if _, err := r.Commit(ctx); err == nil || !strings.Contains(err.Error(), "would both be published at") {
		t.Fatalf("Commit after a colliding relocation = %v, want a clash", err)
	}
	if n := cb.puts["rpm"] + cb.puts["meta"] + cb.copies; n != 0 {
		t.Errorf("Commit wrote or moved %d file(s) before refusing", n)
	}
}

// copy uploads each package as it goes, so a relocation that collides must be
// refused before the second package overwrites the first.
func TestCopyRefusesARelocationThatCollides(t *testing.T) {
	ctx := context.Background()
	srcDir := t.TempDir()
	hello := copyTo(t, helloRPM(), filepath.Join(srcDir, "a", "pkg.rpm"))
	libfoo := copyTo(t, libfooRPM(), filepath.Join(srcDir, "b", "pkg.rpm"))
	pkgs := make([]*repodata.Package, 0, 2)
	for _, f := range []struct{ local, loc string }{{hello, "a/pkg.rpm"}, {libfoo, "b/pkg.rpm"}} {
		p, err := rpmmeta.FromFile(f.local, rpmmeta.DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		p.Location = f.loc
		pkgs = append(pkgs, p)
	}
	srcBE, err := backend.Open(ctx, srcDir)
	if err != nil {
		t.Fatal(err)
	}

	dstDir := t.TempDir()
	dst, _ := newLocalRepo(t, dstDir, true)
	defer dst.Close()
	if _, err := dst.CopyPackagesFrom(ctx, srcBE, pkgs, PackageCopyOptions{Relocate: true}); err == nil {
		t.Fatal("a copy relocating two packages onto pkg.rpm succeeded")
	}
	data, err := os.ReadFile(filepath.Join(dstDir, "pkg.rpm"))
	if err == nil {
		if want, _ := os.ReadFile(hello); string(data) != string(want) {
			t.Error("the second package overwrote the first at pkg.rpm")
		}
	}
}
