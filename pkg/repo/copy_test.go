package repo

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/repodata"
)

// publishedSource builds a two-package repository on disk and returns its
// directory along with a freshly reopened handle on it.
func publishedSource(t *testing.T) (string, *Repo) {
	t.Helper()
	dir := t.TempDir()
	r, _ := newLocalRepo(t, dir, true)
	for _, p := range []string{helloRPM(), libfooRPM()} {
		if _, err := r.AddRPM(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Close()

	src, err := Open(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })
	return dir, src
}

func openBackend(t *testing.T, dir string) backend.Backend {
	t.Helper()
	be, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return be
}

func TestObjectsCoversEveryPublishedFile(t *testing.T) {
	_, src := publishedSource(t)
	objs, listed, err := src.Objects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !listed {
		t.Error("a local backend can list its contents; listed should be true")
	}

	byKind := map[ObjectKind]int{}
	for _, o := range objs {
		byKind[o.Kind]++
	}
	if byKind[ObjectRepomd] != 1 {
		t.Errorf("expected exactly one repomd.xml, got %d", byKind[ObjectRepomd])
	}
	if byKind[ObjectMetadata] != 3 {
		t.Errorf("expected primary/filelists/other, got %d metadata files", byKind[ObjectMetadata])
	}
	if byKind[ObjectPackage] != 2 {
		t.Errorf("expected 2 packages, got %d", byKind[ObjectPackage])
	}

	// repomd.xml must be transferred last so the destination is never a torn
	// repository: a client that reads it must already be able to fetch
	// everything it names.
	if objs[len(objs)-1].Kind != ObjectRepomd {
		t.Errorf("repomd.xml is not last in publish order; got %s", objs[len(objs)-1].Path)
	}

	// Every package and index file carries the checksum the copy verifies against.
	for _, o := range objs {
		if o.Kind == ObjectPackage || o.Kind == ObjectMetadata {
			if !o.verifiable() {
				t.Errorf("%s has no usable checksum (%q/%q)", o.Path, o.ChecksumType, o.Checksum)
			}
		}
	}
}

func TestCopyExactReplicatesByteForByte(t *testing.T) {
	srcDir, src := publishedSource(t)
	dstDir := t.TempDir()
	dst := openBackend(t, dstDir)

	objs, _, err := src.Objects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stats, err := CopyExact(context.Background(), src.Backend(), dst, objs, CopyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != len(objs) || stats.Skipped != 0 {
		t.Errorf("copied %d/%d objects, skipped %d", stats.Copied, len(objs), stats.Skipped)
	}

	for _, o := range objs {
		want, err := os.ReadFile(filepath.Join(srcDir, filepath.FromSlash(o.Path)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dstDir, filepath.FromSlash(o.Path)))
		if err != nil {
			t.Fatalf("%s missing at the destination: %v", o.Path, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs between source and destination", o.Path)
		}
	}

	// The copy is a working repository in its own right.
	copied, err := Open(context.Background(), dstDir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	if copied.Index().Len() != 2 {
		t.Errorf("copied repository holds %d packages, want 2", copied.Index().Len())
	}
}

func TestCopyExactResumeSkipsPackagesAlreadyThere(t *testing.T) {
	_, src := publishedSource(t)
	dstDir := t.TempDir()
	objs, _, err := src.Objects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CopyExact(context.Background(), src.Backend(), openBackend(t, dstDir), objs, CopyOptions{}); err != nil {
		t.Fatal(err)
	}

	stats, err := CopyExact(context.Background(), src.Backend(), openBackend(t, dstDir), objs, CopyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Packages and index files are checksum-matched and skipped; only
	// repomd.xml (and any signature/config, absent here) is rewritten, because
	// nothing records a checksum for it and its length alone proves nothing.
	if stats.Skipped != 5 {
		t.Errorf("resume re-transferred more than it had to: %d skipped, %d copied", stats.Skipped, stats.Copied)
	}
	if stats.Copied != 1 {
		t.Errorf("expected only repomd.xml to be rewritten, %d objects were copied", stats.Copied)
	}
}

func TestCopyExactRejectsCorruptedSource(t *testing.T) {
	srcDir, src := publishedSource(t)
	objs, _, err := src.Objects(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Damage a package after its checksum was recorded, keeping the length so
	// only a real checksum check can catch it.
	var target string
	for _, o := range objs {
		if o.Kind == ObjectPackage {
			target = o.Path
			break
		}
	}
	path := filepath.Join(srcDir, filepath.FromSlash(target))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = CopyExact(context.Background(), src.Backend(), openBackend(t, t.TempDir()), objs, CopyOptions{})
	if err == nil {
		t.Fatal("expected the copy to fail on a package whose checksum no longer matches")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("error should name the checksum mismatch, got: %v", err)
	}
}

func TestCopyExactPruneRemovesExtraneousFiles(t *testing.T) {
	_, src := publishedSource(t)
	dstDir := t.TempDir()
	stray := filepath.Join(dstDir, "leftover.rpm")
	if err := os.WriteFile(stray, []byte("not from the source"), 0o644); err != nil {
		t.Fatal(err)
	}

	objs, _, err := src.Objects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stats, err := CopyExact(context.Background(), src.Backend(), openBackend(t, dstDir), objs, CopyOptions{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Deleted) != 1 || stats.Deleted[0] != "leftover.rpm" {
		t.Errorf("prune removed %v, want [leftover.rpm]", stats.Deleted)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Error("the extraneous file is still present")
	}
}

func TestCopyPackagesFromSelectsAndRelocates(t *testing.T) {
	_, src := publishedSource(t)
	dstDir := t.TempDir()
	dst, err := Open(context.Background(), dstDir, Options{Create: true, now: 2000})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	var selected []*repodata.Package
	for _, p := range src.Index().Packages() {
		if p.Name == "hello" {
			selected = append(selected, p)
		}
	}
	if len(selected) != 1 {
		t.Fatalf("fixture changed: found %d hello packages", len(selected))
	}

	stats, err := dst.CopyPackagesFrom(context.Background(), src.Backend(), selected, PackageCopyOptions{
		LocationPrefix: "rpms",
		Relocate:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 1 {
		t.Fatalf("copied %d packages, want 1", stats.Copied)
	}
	if _, err := dst.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(context.Background(), dstDir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	pkgs := reopened.Index().Packages()
	if len(pkgs) != 1 {
		t.Fatalf("destination holds %d packages, want only the selected one", len(pkgs))
	}
	if pkgs[0].Location != "rpms/hello-2.10-3.noarch.rpm" {
		t.Errorf("package landed at %s, want it under rpms/", pkgs[0].Location)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "rpms", "hello-2.10-3.noarch.rpm")); err != nil {
		t.Errorf("the RPM was not written to its new location: %v", err)
	}
	// The pkgid must still describe the file that was copied.
	if !src.Index().HasPkgID(pkgs[0].PkgID) {
		t.Error("the copied package's checksum does not match the source's")
	}
}

func TestCopyPackagesFromSkipsPackagesAlreadyPresent(t *testing.T) {
	_, src := publishedSource(t)
	dstDir := t.TempDir()
	selected := src.Index().Packages()

	first, err := Open(context.Background(), dstDir, Options{Create: true, now: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.CopyPackagesFrom(context.Background(), src.Backend(), selected, PackageCopyOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, err := Open(context.Background(), dstDir, Options{now: 2001})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	stats, err := second.CopyPackagesFrom(context.Background(), src.Backend(), selected, PackageCopyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 0 || stats.Skipped != len(selected) {
		t.Errorf("resumed copy transferred %d packages and skipped %d; want 0 and %d",
			stats.Copied, stats.Skipped, len(selected))
	}
}

func TestSameRepository(t *testing.T) {
	_, src := publishedSource(t)

	empty := repodata.NewIndex()
	if err := SameRepository(src.Index(), empty); err != nil {
		t.Errorf("an empty destination should qualify as a partial copy: %v", err)
	}

	partial := repodata.NewIndex()
	partial.Add(src.Index().Packages()[0])
	if err := SameRepository(src.Index(), partial); err != nil {
		t.Errorf("a subset should qualify as a partial copy: %v", err)
	}

	foreign := repodata.NewIndex()
	foreign.Add(&repodata.Package{Name: "stranger", Arch: "noarch", Version: "1", Release: "1", PkgID: "deadbeef"})
	err := SameRepository(src.Index(), foreign)
	if err == nil {
		t.Fatal("a destination holding an unrelated package is not a partial copy")
	}
	if !strings.Contains(err.Error(), "stranger") {
		t.Errorf("error should name the offending package, got: %v", err)
	}
}

func TestXMLBasesReportsPinnedPackages(t *testing.T) {
	pkgs := []*repodata.Package{
		{Name: "a", XMLBase: "https://upstream.example.com/el9"},
		{Name: "b", XMLBase: "https://upstream.example.com/el9"},
		{Name: "c"},
	}
	got := XMLBases(pkgs)
	if len(got) != 1 || got[0] != "https://upstream.example.com/el9" {
		t.Errorf("XMLBases = %v, want the single upstream URL", got)
	}
	if len(XMLBases(pkgs[2:])) != 0 {
		t.Error("packages with no xml:base should report none")
	}
}
