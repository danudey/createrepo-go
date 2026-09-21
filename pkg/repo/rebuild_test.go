package repo

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/repodata"
)

// pkg builds a minimal synthetic package for index-level reconciliation tests.
func pkg(name, arch, ver, id string) *repodata.Package {
	return &repodata.Package{
		Name: name, Arch: arch, Version: ver, Release: "1",
		PkgID: id, Location: name + "-" + ver + "-1." + arch + ".rpm", SizePackage: 100,
	}
}

func TestPruneOlderVersions(t *testing.T) {
	dir := t.TempDir()
	r, _ := newLocalRepo(t, dir, true)
	defer r.Close()
	idx := r.Index()
	idx.Add(pkg("hello", "noarch", "2.10", "id-hello-210"))
	idx.Add(pkg("hello", "noarch", "2.9", "id-hello-29"))
	idx.Add(pkg("hello", "noarch", "2.8", "id-hello-28"))
	idx.Add(pkg("libfoo", "x86_64", "1.3.0", "id-libfoo")) // sole version, kept

	rep := r.PruneOlderVersions()
	if len(rep.Removed) != 2 {
		t.Fatalf("dropped %d packages; want 2 (the two older hellos)", len(rep.Removed))
	}
	if idx.Len() != 2 {
		t.Fatalf("index has %d packages after prune; want 2", idx.Len())
	}
	if !idx.HasPkgID("id-hello-210") {
		t.Error("newest hello (2.10) was pruned")
	}
	if !idx.HasPkgID("id-libfoo") {
		t.Error("sole libfoo version was pruned")
	}
	if idx.HasPkgID("id-hello-29") || idx.HasPkgID("id-hello-28") {
		t.Error("an older hello survived the prune")
	}
}

// depPkg builds a synthetic package with explicit provides/requires for
// dependency-aware prune tests.
func depPkg(name, ver, id string, provides, requires []repodata.Entry) *repodata.Package {
	p := pkg(name, "noarch", ver, id)
	p.Provides = provides
	p.Requires = requires
	return p
}

func TestPruneOlderVersionsProtectsDependents(t *testing.T) {
	dir := t.TempDir()
	r, _ := newLocalRepo(t, dir, true)
	defer r.Close()
	idx := r.Index()
	// A has 1.0 and 1.2; B pins A = 1.0, so pruning A-1.0 would break B.
	idx.Add(depPkg("A", "1.0", "id-a10",
		[]repodata.Entry{{Name: "A", Flags: "EQ", Ver: "1.0", Rel: "1"}}, nil))
	idx.Add(depPkg("A", "1.2", "id-a12",
		[]repodata.Entry{{Name: "A", Flags: "EQ", Ver: "1.2", Rel: "1"}}, nil))
	idx.Add(depPkg("B", "1.0", "id-b",
		nil, []repodata.Entry{{Name: "A", Flags: "EQ", Ver: "1.0"}}))

	rep := r.PruneOlderVersions()
	if len(rep.Removed) != 0 {
		t.Errorf("removed %v; want none (A-1.0 is required by B)", rep.Removed)
	}
	if len(rep.Kept) != 1 || rep.Kept[0].Provider.PkgID != "id-a10" || rep.Kept[0].Dependent.PkgID != "id-b" {
		t.Errorf("Kept = %v; want A-1.0 required by B", rep.Kept)
	}
	if !idx.HasPkgID("id-a10") {
		t.Error("A-1.0 was pruned despite dependent B")
	}
}

func TestPruneOlderVersionsBreakDeps(t *testing.T) {
	dir := t.TempDir()
	r, _ := newLocalRepoWithOpts(t, dir, Options{Create: true, now: 1000, PruneBreakDeps: true})
	defer r.Close()
	idx := r.Index()
	idx.Add(depPkg("A", "1.0", "id-a10",
		[]repodata.Entry{{Name: "A", Flags: "EQ", Ver: "1.0", Rel: "1"}}, nil))
	idx.Add(depPkg("A", "1.2", "id-a12",
		[]repodata.Entry{{Name: "A", Flags: "EQ", Ver: "1.2", Rel: "1"}}, nil))
	idx.Add(depPkg("B", "1.0", "id-b",
		nil, []repodata.Entry{{Name: "A", Flags: "EQ", Ver: "1.0"}}))

	rep := r.PruneOlderVersions()
	if len(rep.Removed) != 1 || rep.Removed[0].PkgID != "id-a10" {
		t.Errorf("Removed = %v; want A-1.0 (dropped despite dependent, --prune-break-deps)", rep.Removed)
	}
	if len(rep.Broken) != 1 {
		t.Errorf("Broken = %v; want 1 broken dependency", rep.Broken)
	}
	if idx.HasPkgID("id-a10") {
		t.Error("A-1.0 should have been pruned with PruneBreakDeps")
	}
}

func TestRelocateAll(t *testing.T) {
	dir := t.TempDir()
	r, _ := newLocalRepo(t, dir, true)
	defer r.Close()
	idx := r.Index()
	idx.Add(pkg("hello", "noarch", "2.10", "id1"))   // hello-2.10-1.noarch.rpm
	idx.Add(pkg("libfoo", "x86_64", "1.3.0", "id2")) // libfoo-1.3.0-1.x86_64.rpm

	if moved := r.RelocateAll("pool"); moved != 2 {
		t.Fatalf("first relocate moved %d; want 2", moved)
	}
	for _, p := range idx.Packages() {
		if filepath.Dir(p.Location) != "pool" {
			t.Errorf("%s not relocated under pool/: %s", p.NEVRA(), p.Location)
		}
	}
	// Idempotent: relocating to the same prefix again moves nothing.
	if moved := r.RelocateAll("pool"); moved != 0 {
		t.Errorf("second relocate moved %d; want 0 (idempotent)", moved)
	}
	// Relocating to the repo root strips the prefix.
	if moved := r.RelocateAll(""); moved != 2 {
		t.Errorf("relocate to root moved %d; want 2", moved)
	}
	for _, p := range idx.Packages() {
		if filepath.Dir(p.Location) != "." {
			t.Errorf("%s not at repo root: %s", p.NEVRA(), p.Location)
		}
	}
}

func TestReplaceFromFile(t *testing.T) {
	dir := t.TempDir()
	r, _ := newLocalRepo(t, dir, true)
	defer r.Close()
	orig, err := r.AddRPM(helloRPM())
	if err != nil {
		t.Fatal(err)
	}

	newLoc := "pool/hello-2.10-3.noarch.rpm"
	replaced, err := r.ReplaceFromFile(helloRPM(), newLoc)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.Location != newLoc {
		t.Errorf("replaced location = %q; want %q", replaced.Location, newLoc)
	}
	if r.Index().Len() != 1 {
		t.Errorf("index has %d packages; want 1 (same NEVRA superseded)", r.Index().Len())
	}
	if r.Index().ByLocation(orig.Location) != nil {
		t.Errorf("old location %q still indexed after replace", orig.Location)
	}
	if r.Index().ByLocation(newLoc) == nil {
		t.Errorf("new location %q not indexed after replace", newLoc)
	}
}

// TestRebuildRelocateFromOrphan exercises the buildPlan pass that moves an
// existing (non-pending) package to a new location via a server-side copy —
// the mechanism behind `rebuild --location-prefix`.
func TestRebuildRelocateFromOrphan(t *testing.T) {
	dir := t.TempDir()
	// Publish hello under the default (empty) prefix at the repo root.
	r, _ := newLocalRepo(t, dir, true)
	if _, err := r.AddRPM(helloRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Close()

	rootLoc := filepath.Base(helloRPM())
	poolLoc := filepath.Join("pool", rootLoc)

	// Reopen and relocate without staging any local file.
	inner, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	cb := wrap(inner)
	r2, err := OpenWith(context.Background(), cb, Options{now: 1000})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if moved := r2.RelocateAll("pool"); moved != 1 {
		t.Fatalf("relocate moved %d; want 1", moved)
	}
	plan, err := r2.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Uploads) != 0 || cb.puts["rpm"] != 0 {
		t.Errorf("relocation re-uploaded (uploads=%d, puts=%d); want 0 (server-side move)", len(plan.Uploads), cb.puts["rpm"])
	}
	if len(plan.Copies) != 1 || plan.Copies[0].From != rootLoc || plan.Copies[0].To != poolLoc {
		t.Errorf("plan.Copies = %+v; want one %s -> %s", plan.Copies, rootLoc, poolLoc)
	}
	if _, err := os.Stat(filepath.Join(dir, rootLoc)); !os.IsNotExist(err) {
		t.Errorf("old location still present after relocation (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, poolLoc)); err != nil {
		t.Errorf("new location missing after relocation: %v", err)
	}
}

func TestRebuildCleanups(t *testing.T) {
	dir := t.TempDir()
	r, _ := newLocalRepo(t, dir, true)
	if _, err := r.AddRPM(helloRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Close()

	// Drop a stray RPM and a stray repodata file that nothing references.
	strayRPM := "stray-9.9-9.noarch.rpm"
	if err := os.WriteFile(filepath.Join(dir, strayRPM), []byte("not really an rpm"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Repo-relative keys are always forward-slashed, whatever the local
	// separator, because that is what a plan reports and what a backend stores.
	// filepath.FromSlash turns one back into a local path.
	staleMeta := "repodata/leftover-primary.xml.gz"
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(staleMeta)), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	r2, _ := newLocalRepoWithOpts(t, dir, Options{now: 1000, RemoveUnreferencedRPMs: true, RemoveStaleMetadata: true})
	defer r2.Close()
	plan, err := r2.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.UnreferencedRPMs) != 1 || plan.UnreferencedRPMs[0] != strayRPM {
		t.Errorf("plan.UnreferencedRPMs = %v; want [%s]", plan.UnreferencedRPMs, strayRPM)
	}
	if len(plan.StaleMetadata) != 1 || plan.StaleMetadata[0] != staleMeta {
		t.Errorf("plan.StaleMetadata = %v; want [%s]", plan.StaleMetadata, staleMeta)
	}
	if _, err := os.Stat(filepath.Join(dir, strayRPM)); !os.IsNotExist(err) {
		t.Errorf("stray RPM not deleted (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(staleMeta))); !os.IsNotExist(err) {
		t.Errorf("stale metadata not deleted (err=%v)", err)
	}
	// The referenced RPM must survive.
	if _, err := os.Stat(filepath.Join(dir, filepath.Base(helloRPM()))); err != nil {
		t.Errorf("referenced RPM was deleted: %v", err)
	}
}

func TestTotalSize(t *testing.T) {
	dir := t.TempDir()
	// Use the raw local backend (a Lister); the counting wrapper is not.
	r, _ := newLocalRepoWithOpts(t, dir, Options{Create: true, now: 1000})
	defer r.Close()
	if _, err := r.AddRPM(helloRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	bytes, files, listed, err := r.TotalSize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !listed {
		t.Error("local backend should report listed=true")
	}
	if files == 0 || bytes == 0 {
		t.Errorf("TotalSize = %d bytes across %d files; want non-zero", bytes, files)
	}
}

// newLocalRepoWithOpts opens a local repo with caller-supplied options.
func newLocalRepoWithOpts(t *testing.T, dir string, opts Options) (*Repo, backend.Backend) {
	t.Helper()
	inner, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := OpenWith(context.Background(), inner, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r, inner
}
