package publish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danudey/createrepo-go/pkg/backend"
)

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func repoWith(t *testing.T, files map[string]string) (backend.Backend, string) {
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
	return be, dir
}

// statOnly hides the hashing and copying capabilities, as plain object storage
// without them would: only sizes can be compared, and nothing can move.
type statOnly struct{ inner backend.Backend }

func (b statOnly) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	return b.inner.Get(ctx, p)
}

func (b statOnly) Stat(ctx context.Context, p string) (*backend.FileInfo, error) {
	return b.inner.Stat(ctx, p)
}

func (b statOnly) Put(ctx context.Context, p string, r io.Reader, n int64) error {
	return b.inner.Put(ctx, p, r, n)
}
func (b statOnly) Delete(ctx context.Context, p string) error { return b.inner.Delete(ctx, p) }
func (b statOnly) String() string                             { return "stat-only" }

func TestPlanSkipsAMatchingFileWhateverTheChecksumsCase(t *testing.T) {
	be, _ := repoWith(t, map[string]string{"pool/a.deb": "aaa"})
	plan, err := PlanFiles(context.Background(), be, State{
		Referenced: map[string]Expect{"pool/a.deb": {Size: 3, SHA256: strings.ToUpper(sum("aaa")), ID: "a"}},
		Pending:    map[string]string{"pool/a.deb": "/local/a.deb"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Skipped) != 1 || len(plan.Uploads) != 0 {
		t.Errorf("plan = %+v, want a.deb skipped as already present", plan)
	}
}

func TestPlanRefusesADifferentFileWithoutForce(t *testing.T) {
	be, _ := repoWith(t, map[string]string{"pool/a.deb": "aaa"})
	state := State{
		Referenced: map[string]Expect{"pool/a.deb": {Size: 3, SHA256: sum("bbb"), ID: "a"}},
		Pending:    map[string]string{"pool/a.deb": "/local/a.deb"},
	}
	if _, err := PlanFiles(context.Background(), be, state); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Errorf("PlanFiles = %v, want a different-content error", err)
	}
	state.Force = true
	plan, err := PlanFiles(context.Background(), be, state)
	if err != nil || len(plan.Uploads) != 1 {
		t.Errorf("with Force: plan = %+v, %v; want one upload", plan, err)
	}
}

// Without a checksum the size has to stand in, and it has to actually agree:
// an existing file of a different size is not the one being published.
func TestPlanComparesSizesWhenItCannotHash(t *testing.T) {
	inner, _ := repoWith(t, map[string]string{"pool/a.deb": "aaa"})
	be := statOnly{inner}
	for _, tc := range []struct {
		size   int64
		wantOK bool
	}{{3, true}, {0, false}, {4, false}} {
		plan, err := PlanFiles(context.Background(), be, State{
			Referenced: map[string]Expect{"pool/a.deb": {Size: tc.size, SHA256: sum("aaa"), ID: "a"}},
			Pending:    map[string]string{"pool/a.deb": "/local/a.deb"},
		})
		if tc.wantOK && (err != nil || len(plan.Skipped) != 1) {
			t.Errorf("size %d: plan = %+v, %v; want a skip", tc.size, plan, err)
		}
		if !tc.wantOK && err == nil {
			t.Errorf("size %d: PlanFiles accepted a file of a different size", tc.size)
		}
	}
}

func TestPlanRelocatesMovedFilesAndCollectsOrphans(t *testing.T) {
	be, _ := repoWith(t, map[string]string{"old/a.deb": "aaa", "old/gone.deb": "x"})
	plan, err := PlanFiles(context.Background(), be, State{
		Referenced: map[string]Expect{"pool/a.deb": {Size: 3, SHA256: sum("aaa"), ID: "id-a"}},
		Original:   map[string]string{"old/a.deb": "id-a", "old/gone.deb": "id-gone"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Copies) != 1 || plan.Copies[0] != (CopyAction{From: "old/a.deb", To: "pool/a.deb", Size: 3}) {
		t.Errorf("copies = %+v, want old/a.deb relocated to pool/a.deb", plan.Copies)
	}
	if strings.Join(plan.Deleted, ",") != "old/a.deb,old/gone.deb" {
		t.Errorf("deleted = %v, want both old files collected", plan.Deleted)
	}
}

func TestPlanNamesAFileItCannotPlace(t *testing.T) {
	inner, _ := repoWith(t, map[string]string{"old/a.deb": "aaa"})
	_, err := PlanFiles(context.Background(), statOnly{inner}, State{
		Referenced: map[string]Expect{"pool/a.deb": {Size: 3, ID: "id-a"}},
		Original:   map[string]string{"old/a.deb": "id-a"},
		Describe:   func(p string) string { return "hello-1.0 at " + p },
	})
	if err == nil || !strings.Contains(err.Error(), "cannot place hello-1.0 at pool/a.deb") {
		t.Errorf("PlanFiles = %v, want it to name the package it cannot place", err)
	}
}

func TestUnreferenced(t *testing.T) {
	be, _ := repoWith(t, map[string]string{"pool/a": "", "pool/b": "", "pool/c": "", "other/d": ""})
	got, err := Unreferenced(context.Background(), be.(backend.Lister), "pool/",
		func(p string) bool { return p == "pool/a" }, []string{"pool/c"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "pool/b" {
		t.Errorf("Unreferenced = %v, want [pool/b]", got)
	}
}
