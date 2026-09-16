package repo

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/danudey/createrepo-go/pkg/backend"
)

// appendByte changes a file's content and length while leaving it a parseable
// RPM, standing in for a package rebuilt and republished under the same name.
func appendByte(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
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

// reopen reopens a published repository for a fresh operation.
func reopen(t *testing.T, be backend.Backend) *Repo {
	t.Helper()
	r, err := OpenWith(context.Background(), be, Options{now: 2000})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRefreshCorrectsMetadataThatNoLongerMatchesTheFile(t *testing.T) {
	dir, ids := verifiableRepo(t)
	path := rpmPath(t, dir, ids, "hello")
	appendByte(t, path)

	be, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	// The published metadata is now wrong about that package.
	stale := reopen(t, be)
	if res, _ := stale.Verify(context.Background(), VerifyOptions{}); res.OK() {
		t.Fatal("the repository should not verify before the refresh")
	}
	stale.Close()

	r := reopen(t, be)
	defer r.Close()
	got, err := r.RefreshFromPackages(context.Background(), RefreshOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Read != 2 || got.Changed != 1 {
		t.Errorf("read %d / changed %d, want 2 / 1", got.Read, got.Changed)
	}
	if got.Downloaded != 0 {
		t.Errorf("a local repository should be re-read in place, but %d package(s) were transferred", got.Downloaded)
	}
	if _, err := r.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	// And now it does verify.
	after := reopen(t, be)
	defer after.Close()
	res, err := after.Verify(context.Background(), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Errorf("the repository should verify after the refresh: %v", res.Problems)
	}
	if after.Index().Len() != 2 {
		t.Errorf("the refresh changed the package count to %d, want 2", after.Index().Len())
	}
}

// Published metadata should never carry two records for one file, but a bad
// publish upstream can leave a stale record beside the current one. Re-reading
// the file collapses them onto the record that describes it.
func TestRefreshDropsDuplicateRecordsForOneFile(t *testing.T) {
	dir, _ := verifiableRepo(t)
	be, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	// Publish a second, wrong record for a file that already has one.
	r := reopen(t, be)
	real := r.Index().Packages()[0]
	ghost := *real
	ghost.PkgID = strings.Repeat("a", 64) // a checksum no file has
	r.Index().Add(&ghost)
	if _, err := r.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Close()

	dup := reopen(t, be)
	if dup.Index().Len() != 3 {
		t.Fatalf("fixture: expected 3 records (one duplicated), got %d", dup.Index().Len())
	}
	got, err := dup.RefreshFromPackages(context.Background(), RefreshOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Duplicates != 1 {
		t.Errorf("dropped %d duplicate records, want 1", got.Duplicates)
	}
	if dup.Index().Len() != 2 {
		t.Errorf("index holds %d packages after the refresh, want 2", dup.Index().Len())
	}
	plan, err := dup.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The duplicate named a file another record still names, so nothing is
	// unreferenced and no RPM may be deleted.
	if len(plan.DeletedRPMs) != 0 {
		t.Errorf("dropping a duplicate record deleted %v; it must delete nothing", plan.DeletedRPMs)
	}
	dup.Close()

	after := reopen(t, be)
	defer after.Close()
	res, _ := after.Verify(context.Background(), VerifyOptions{})
	if !res.OK() {
		t.Errorf("the repository should verify once the duplicate is gone: %v", res.Problems)
	}
}

// Two distinct locations holding byte-identical packages cannot both be in an
// index keyed by checksum. Dropping either would leave its RPM unreferenced and
// so due for deletion, so the refresh must change nothing and say so.
func TestRefreshLeavesIdenticalPackagesAtTwoLocationsAlone(t *testing.T) {
	dir, ids := verifiableRepo(t)
	hello := rpmPath(t, dir, ids, "hello")
	libfoo := rpmPath(t, dir, ids, "libfoo")

	// Overwrite one package's file with the other's content.
	data, err := os.ReadFile(libfoo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hello, data, 0o644); err != nil {
		t.Fatal(err)
	}

	be, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	r := reopen(t, be)
	defer r.Close()

	got, err := r.RefreshFromPackages(context.Background(), RefreshOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Conflicts) != 1 {
		t.Fatalf("expected one conflict to be reported, got %v", got.Conflicts)
	}
	for _, want := range []string{"hello", "libfoo"} {
		if !strings.Contains(got.Conflicts[0], want) {
			t.Errorf("the conflict should name both locations, got %q", got.Conflicts[0])
		}
	}
	if r.Index().Len() != 2 {
		t.Fatalf("both entries must survive, got %d", r.Index().Len())
	}
	plan, err := r.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.DeletedRPMs) != 0 {
		t.Errorf("the refresh scheduled %v for deletion; it must not remove a stored package", plan.DeletedRPMs)
	}
}

func TestRefreshLeavesMissingPackagesAsPublished(t *testing.T) {
	dir, ids := verifiableRepo(t)
	gone := rpmPath(t, dir, ids, "hello")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	be, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	r := reopen(t, be)
	defer r.Close()

	got, err := r.RefreshFromPackages(context.Background(), RefreshOptions{})
	if err != nil {
		t.Fatalf("a missing package must not abort the refresh: %v", err)
	}
	if len(got.Missing) != 1 || !strings.Contains(got.Missing[0], "hello") {
		t.Errorf("missing package not reported: %v", got.Missing)
	}
	if got.Read != 1 {
		t.Errorf("read %d packages, want the 1 that is still there", got.Read)
	}
	// The entry stays, so verify still reports the package as missing rather
	// than the refresh quietly dropping it from the repository.
	if r.Index().Len() != 2 {
		t.Errorf("index holds %d packages, want both entries kept", r.Index().Len())
	}
}

func TestRefreshDownloadsWhenPackagesAreNotLocalFiles(t *testing.T) {
	dir, ids := verifiableRepo(t)
	appendByte(t, rpmPath(t, dir, ids, "hello"))

	local, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	// recordingBackend embeds the Backend interface, so it does not expose
	// LocalPath: to the repository it looks like remote storage.
	remote := &recordingBackend{Backend: local, recorded: ids}
	if backend.IsLocal(remote) {
		t.Fatal("fixture: the stand-in backend should not look local")
	}

	r := reopen(t, remote)
	defer r.Close()
	n, size := r.RefreshEstimate()
	if n != 2 || size <= 0 {
		t.Errorf("estimate says %d packages / %d bytes, want both packages counted", n, size)
	}

	got, err := r.RefreshFromPackages(context.Background(), RefreshOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Downloaded != 2 || got.BytesRead <= 0 {
		t.Errorf("downloaded %d packages / %d bytes, want both transferred", got.Downloaded, got.BytesRead)
	}
	if got.Changed != 1 {
		t.Errorf("corrected %d packages, want 1", got.Changed)
	}
}

func TestRefreshKeepsLocationAndBaseURL(t *testing.T) {
	dir, ids := verifiableRepo(t)
	appendByte(t, rpmPath(t, dir, ids, "hello"))

	be, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	r := reopen(t, be)
	defer r.Close()

	const base = "https://upstream.example.com/el9/"
	want := map[string]string{}
	for _, p := range r.Index().Packages() {
		p.XMLBase = base
		want[p.Name] = p.Location
	}
	if _, err := r.RefreshFromPackages(context.Background(), RefreshOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range r.Index().Packages() {
		if p.Location != want[p.Name] {
			t.Errorf("%s moved to %s, want %s", p.Name, p.Location, want[p.Name])
		}
		if p.XMLBase != base {
			t.Errorf("%s lost its xml:base (%q)", p.Name, p.XMLBase)
		}
	}
}
