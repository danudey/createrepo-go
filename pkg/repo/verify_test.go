package repo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danudey/createrepo-go/pkg/backend"
)

// recordingBackend stands in for an object store: it answers Hash from a
// checksum noted when the object was uploaded rather than by re-reading the
// stored bytes, so its answer survives the content being changed underneath it.
type recordingBackend struct {
	backend.Backend
	recorded map[string]string // relpath -> sha256 as uploaded
}

func (b *recordingBackend) HashesContent() bool { return false }

func (b *recordingBackend) Hash(_ context.Context, relpath, algo string) (string, bool, error) {
	if algo != "sha256" {
		return "", false, nil
	}
	sum, ok := b.recorded[relpath]
	return sum, ok, nil
}

// verifiableRepo publishes the reference RPMs into a temp directory and returns
// the directory plus the pkgid the metadata records for each location.
func verifiableRepo(t *testing.T) (string, map[string]string) {
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
	ids := map[string]string{}
	for _, p := range r.Index().Packages() {
		ids[p.Location] = p.PkgID
	}
	r.Close()
	return dir, ids
}

// rpmPath returns the on-disk path of the published RPM whose location
// contains name, so tests do not depend on the repository's layout.
func rpmPath(t *testing.T, dir string, ids map[string]string, name string) string {
	t.Helper()
	for loc := range ids {
		if strings.Contains(loc, name) {
			return filepath.Join(dir, filepath.FromSlash(loc))
		}
	}
	t.Fatalf("no published package matching %q in %v", name, ids)
	return ""
}

// corrupt flips a byte in the middle of a file, leaving its length alone so
// only a checksum can catch it.
func corrupt(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func openVerify(t *testing.T, be backend.Backend, opt VerifyOptions) *VerifyResult {
	t.Helper()
	r, err := OpenWith(context.Background(), be, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	res, err := r.Verify(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestVerifyProvesChecksumsFromContentOnALocalBackend(t *testing.T) {
	dir, _ := verifiableRepo(t)
	be, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	res := openVerify(t, be, VerifyOptions{})
	if !res.OK() {
		t.Fatalf("a freshly published repository should verify: %v", res.Problems)
	}
	if res.ChecksumVerified != 2 {
		t.Errorf("verified %d checksums from content, want 2 (local disk hashes for free)", res.ChecksumVerified)
	}
	if res.ChecksumUnconfirmed != 0 || res.ChecksumRecorded != 0 {
		t.Errorf("nothing should be left unproved on local disk: %+v", res)
	}
	if res.Downloaded != 0 {
		t.Errorf("a local backend should need no downloads, got %d", res.Downloaded)
	}
}

func TestVerifyDetectsCorruptionOfTheSameLength(t *testing.T) {
	dir, ids := verifiableRepo(t)
	corrupt(t, rpmPath(t, dir, ids, "hello"))

	be, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	res := openVerify(t, be, VerifyOptions{})
	if res.OK() {
		t.Fatal("a package whose content changed should not verify")
	}
	if len(res.Problems) != 1 {
		t.Fatalf("expected one problem, got %v", res.Problems)
	}
	if !strings.Contains(res.Problems[0].Detail, "checksum") {
		t.Errorf("the problem should name the checksum mismatch: %v", res.Problems[0])
	}
}

func TestVerifyReportsMissingAndResizedPackages(t *testing.T) {
	dir, ids := verifiableRepo(t)
	if err := os.Remove(rpmPath(t, dir, ids, "hello")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rpmPath(t, dir, ids, "libfoo"), []byte("truncated"), 0o644); err != nil {
		t.Fatal(err)
	}

	be, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	res := openVerify(t, be, VerifyOptions{})
	if len(res.Problems) < 2 {
		t.Fatalf("expected a problem for each damaged package, got %v", res.Problems)
	}
	joined := ""
	for _, p := range res.Problems {
		joined += p.String() + "\n"
	}
	if !strings.Contains(joined, "missing") {
		t.Errorf("the deleted package was not reported as missing:\n%s", joined)
	}
	if !strings.Contains(joined, "size") {
		t.Errorf("the truncated package's size was not reported:\n%s", joined)
	}
}

// A store that only plays back the checksum it recorded at upload time cannot
// notice content that changed afterwards. Verify must say so rather than call
// it verified, and --checksums must catch the corruption by reading the object.
func TestVerifyDistinguishesRecordedChecksumsFromProvenOnes(t *testing.T) {
	dir, ids := verifiableRepo(t)
	local, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	store := &recordingBackend{Backend: local, recorded: ids}

	// Undamaged: the recorded checksums agree with the metadata.
	res := openVerify(t, store, VerifyOptions{})
	if !res.OK() {
		t.Fatalf("an intact repository should verify: %v", res.Problems)
	}
	if res.ChecksumRecorded != 2 || res.ChecksumVerified != 0 {
		t.Errorf("checksums should be reported as recorded, not proved: %+v", res)
	}

	// Now change the content. The recorded checksum still matches, so the cheap
	// check cannot tell — and must not claim to have proved anything.
	corrupt(t, rpmPath(t, dir, ids, "hello"))
	res = openVerify(t, store, VerifyOptions{})
	if !res.OK() {
		t.Errorf("the recorded checksum cannot see this corruption; got %v", res.Problems)
	}
	if res.ChecksumVerified != 0 {
		t.Error("a recorded checksum must never be counted as verified from content")
	}

	// Reading the object finds it.
	res = openVerify(t, store, VerifyOptions{Checksums: ChecksumContent})
	if res.OK() {
		t.Fatal("--checksums should have caught the changed content")
	}
	if res.Downloaded != 2 {
		t.Errorf("content mode should have read both packages, read %d", res.Downloaded)
	}
	if res.BytesRead == 0 {
		t.Error("content mode reported no bytes read")
	}
	if !strings.Contains(res.Problems[0].Detail, "content checksum") {
		t.Errorf("expected a content checksum mismatch, got %v", res.Problems[0])
	}
}

func TestDownloadEstimateOnlyCountsWhatMustBeTransferred(t *testing.T) {
	dir, ids := verifiableRepo(t)
	local, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	r, err := OpenWith(context.Background(), local, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n, size := r.DownloadEstimate(); n != 0 || size != 0 {
		t.Errorf("a backend that hashes content needs no download, got %d packages / %d bytes", n, size)
	}
	r.Close()

	store, err := OpenWith(context.Background(), &recordingBackend{Backend: local, recorded: ids}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	n, size := store.DownloadEstimate()
	if n != 2 || size <= 0 {
		t.Errorf("a recording store must transfer every package: got %d packages / %d bytes", n, size)
	}
}
