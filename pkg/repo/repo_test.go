package repo

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/danudey/createrepo-go/pkg/backend"
)

const refRPMDir = "../../reference/rpmbuild/RPMS"

func helloRPM() string  { return filepath.Join(refRPMDir, "noarch/hello-2.10-3.noarch.rpm") }
func libfooRPM() string { return filepath.Join(refRPMDir, "x86_64/libfoo-1.3.0-1.x86_64.rpm") }

// countingBackend wraps a Backend and records access counts per operation,
// keyed by a coarse path class, so tests can assert on transfer behavior.
type countingBackend struct {
	inner   backend.Backend
	mu      sync.Mutex
	gets    map[string]int
	puts    map[string]int
	stats   map[string]int
	copies  int
	deletes map[string]int
}

func wrap(inner backend.Backend) *countingBackend {
	return &countingBackend{inner: inner, gets: map[string]int{}, puts: map[string]int{}, stats: map[string]int{}, deletes: map[string]int{}}
}

func class(relpath string) string {
	if filepath.Ext(relpath) == ".rpm" {
		return "rpm"
	}
	return "meta"
}

func (c *countingBackend) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	c.mu.Lock()
	c.gets[class(p)]++
	c.mu.Unlock()
	return c.inner.Get(ctx, p)
}

func (c *countingBackend) Stat(ctx context.Context, p string) (*backend.FileInfo, error) {
	c.mu.Lock()
	c.stats[class(p)]++
	c.mu.Unlock()
	return c.inner.Stat(ctx, p)
}

func (c *countingBackend) Put(ctx context.Context, p string, r io.Reader, n int64) error {
	c.mu.Lock()
	c.puts[class(p)]++
	c.mu.Unlock()
	return c.inner.Put(ctx, p, r, n)
}

func (c *countingBackend) Delete(ctx context.Context, p string) error {
	c.mu.Lock()
	c.deletes[class(p)]++
	c.mu.Unlock()
	return c.inner.Delete(ctx, p)
}
func (c *countingBackend) String() string { return c.inner.String() }

// Copy forwards to the inner backend's Copier so relocation works, counting the
// server-side copies performed.
func (c *countingBackend) Copy(ctx context.Context, from, to string) error {
	cp, ok := c.inner.(backend.Copier)
	if !ok {
		return backend.ErrNotExist
	}
	c.mu.Lock()
	c.copies++
	c.mu.Unlock()
	return cp.Copy(ctx, from, to)
}

// Hash forwards to the inner backend's RemoteHasher so checksum validation works.
func (c *countingBackend) Hash(ctx context.Context, p, algo string) (string, bool, error) {
	if h, ok := c.inner.(backend.RemoteHasher); ok {
		c.mu.Lock()
		c.gets["hash-"+class(p)]++
		c.mu.Unlock()
		return h.Hash(ctx, p, algo)
	}
	return "", false, nil
}

func newLocalRepo(t *testing.T, dir string, create bool) (*Repo, *countingBackend) {
	t.Helper()
	inner, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	cb := wrap(inner)
	r, err := OpenWith(context.Background(), cb, Options{Create: create, now: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return r, cb
}

func TestPublishRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r, _ := newLocalRepo(t, dir, true)
	if _, err := r.AddRPM(helloRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddRPM(libfooRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Close()

	// Reopen and confirm both packages are present with their files/changelogs.
	r2, _ := newLocalRepo(t, dir, false)
	defer r2.Close()
	if r2.Index().Len() != 2 {
		t.Fatalf("reopened index has %d packages, want 2", r2.Index().Len())
	}
	for _, p := range r2.Index().Packages() {
		if len(p.Files) == 0 {
			t.Errorf("%s lost its file list on round-trip", p.NEVRA())
		}
		if p.Name == "hello" && len(p.Changelogs) != 2 {
			t.Errorf("hello changelogs = %d, want 2", len(p.Changelogs))
		}
	}
}

func TestMinimalTransferOnAdd(t *testing.T) {
	dir := t.TempDir()
	// Seed a repo with libfoo.
	r, _ := newLocalRepo(t, dir, true)
	if _, err := r.AddRPM(libfooRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Close()

	// Add hello to the existing repo; assert we never read an RPM.
	r2, cb := newLocalRepo(t, dir, false)
	defer r2.Close()
	if _, err := r2.AddRPM(helloRPM()); err != nil {
		t.Fatal(err)
	}
	plan, err := r2.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cb.gets["rpm"] != 0 {
		t.Errorf("read %d RPM(s) during add; want 0 (RPMs must never be downloaded)", cb.gets["rpm"])
	}
	if cb.puts["rpm"] != 1 {
		t.Errorf("uploaded %d RPM(s); want exactly 1 (only the new package)", cb.puts["rpm"])
	}
	if len(plan.Uploads) != 1 || plan.Uploads[0].Size == 0 {
		t.Errorf("plan uploads = %+v, want exactly the new RPM", plan.Uploads)
	}
}

func TestIdempotentReAdd(t *testing.T) {
	dir := t.TempDir()
	r, _ := newLocalRepo(t, dir, true)
	if _, err := r.AddRPM(helloRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Close()

	r2, _ := newLocalRepo(t, dir, false)
	defer r2.Close()
	if _, err := r2.AddRPM(helloRPM()); err != nil {
		t.Fatal(err)
	}
	plan, err := r2.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Uploads) != 0 {
		t.Errorf("re-adding identical RPM uploaded %d file(s); want 0", len(plan.Uploads))
	}
	if len(plan.Skipped) != 1 {
		t.Errorf("re-add skipped %d; want 1", len(plan.Skipped))
	}
}

func TestForceAlwaysUploads(t *testing.T) {
	dir := t.TempDir()
	r, _ := newLocalRepo(t, dir, true)
	if _, err := r.AddRPM(helloRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Close()

	// Re-add the identical RPM with Force set; it must upload, not skip, even
	// though the remote file is byte-identical.
	inner, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := OpenWith(context.Background(), wrap(inner), Options{now: 1000, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if _, err := r2.AddRPM(helloRPM()); err != nil {
		t.Fatal(err)
	}
	plan, err := r2.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Uploads) != 1 {
		t.Errorf("--force re-add uploaded %d file(s); want 1 (force must overwrite)", len(plan.Uploads))
	}
	if len(plan.Skipped) != 0 {
		t.Errorf("--force re-add skipped %d file(s); want 0", len(plan.Skipped))
	}
}

func TestRelocationMovesInsteadOfReupload(t *testing.T) {
	dir := t.TempDir()
	// Publish hello at the repo root.
	r, _ := newLocalRepo(t, dir, true)
	if _, err := r.AddRPM(helloRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Close()

	rootLoc := filepath.Base(helloRPM())
	prefixLoc := filepath.Join("Packages", rootLoc)

	// Re-add the identical RPM under a new --location-prefix.
	inner, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	cb := wrap(inner)
	r2, err := OpenWith(context.Background(), cb, Options{now: 1000, LocationPrefix: "Packages"})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if _, err := r2.AddRPM(helloRPM()); err != nil {
		t.Fatal(err)
	}
	plan, err := r2.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// The file must be relocated server-side, not re-uploaded.
	if len(plan.Uploads) != 0 {
		t.Errorf("relocation uploaded %d file(s); want 0", len(plan.Uploads))
	}
	if cb.puts["rpm"] != 0 {
		t.Errorf("relocation transferred %d RPM(s); want 0 (server-side copy)", cb.puts["rpm"])
	}
	if len(plan.Copies) != 1 || plan.Copies[0].From != rootLoc || plan.Copies[0].To != prefixLoc {
		t.Errorf("plan.Copies = %+v; want one %s -> %s", plan.Copies, rootLoc, prefixLoc)
	}
	if cb.copies != 1 {
		t.Errorf("performed %d server-side copies; want 1", cb.copies)
	}

	// The old copy must be gone and the new one present.
	if _, err := os.Stat(filepath.Join(dir, rootLoc)); !os.IsNotExist(err) {
		t.Errorf("old location %s still present after relocation (err=%v)", rootLoc, err)
	}
	if _, err := os.Stat(filepath.Join(dir, prefixLoc)); err != nil {
		t.Errorf("new location %s missing after relocation: %v", prefixLoc, err)
	}

	// Reopening shows exactly one package, at the new location.
	r3, _ := newLocalRepo(t, dir, false)
	defer r3.Close()
	pkgs := r3.Index().Packages()
	if len(pkgs) != 1 || pkgs[0].Location != prefixLoc {
		got := make([]string, len(pkgs))
		for i, p := range pkgs {
			got[i] = p.Location
		}
		t.Errorf("reopened index = %d package(s) at %v; want 1 at %s", len(pkgs), got, prefixLoc)
	}
}

func TestRemoveGarbageCollectsBlob(t *testing.T) {
	dir := t.TempDir()
	r, _ := newLocalRepo(t, dir, true)
	if _, err := r.AddRPM(helloRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddRPM(libfooRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Close()

	// Removing hello must delete its blob automatically (no flag required).
	r2, _ := newLocalRepo(t, dir, false)
	defer r2.Close()
	if removed := r2.Remove("hello", "", ""); len(removed) != 1 {
		t.Fatalf("Remove(hello) removed %d; want 1", len(removed))
	}
	plan, err := r2.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	helloLoc := filepath.Base(helloRPM())
	if len(plan.DeletedRPMs) != 1 || plan.DeletedRPMs[0] != helloLoc {
		t.Errorf("plan.DeletedRPMs = %v; want [%s]", plan.DeletedRPMs, helloLoc)
	}
	if _, err := os.Stat(filepath.Join(dir, helloLoc)); !os.IsNotExist(err) {
		t.Errorf("removed blob %s still present (err=%v)", helloLoc, err)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.Base(libfooRPM()))); err != nil {
		t.Errorf("surviving package's blob missing: %v", err)
	}
}

func TestObsoleteMetadataGarbageCollected(t *testing.T) {
	dir := t.TempDir()
	r, _ := newLocalRepo(t, dir, true)
	if _, err := r.AddRPM(helloRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Close()

	r2, _ := newLocalRepo(t, dir, false)
	defer r2.Close()
	if _, err := r2.AddRPM(libfooRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Exactly one primary/filelists/other should remain in repodata.
	metas, _ := filepath.Glob(filepath.Join(dir, "repodata", "*-primary.xml.gz"))
	if len(metas) != 1 {
		t.Errorf("found %d primary metadata files after update; want 1 (old should be GC'd): %v", len(metas), metas)
	}
}
