package integrity

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
	"github.com/danudey/createrepo-go/pkg/progress"
)

var words = Wording{Records: "the index", Missing: "missing", Object: "the file"}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func localRepo(t *testing.T, files map[string]string) backend.Backend {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, rel)
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

// blind hides every optional capability, as plain HTTP would: no hashing, so
// only a download can prove content.
type blind struct{ inner backend.Backend }

func (b blind) Get(ctx context.Context, p string) (io.ReadCloser, error) { return b.inner.Get(ctx, p) }

func (b blind) Stat(ctx context.Context, p string) (*backend.FileInfo, error) {
	return b.inner.Stat(ctx, p)
}

func (b blind) Put(ctx context.Context, p string, r io.Reader, n int64) error {
	return b.inner.Put(ctx, p, r, n)
}
func (b blind) Delete(ctx context.Context, p string) error { return b.inner.Delete(ctx, p) }
func (b blind) String() string                             { return "blind" }

func TestCheck(t *testing.T) {
	be := localRepo(t, map[string]string{"a.deb": "hello"})
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		be      backend.Backend
		file    File
		mode    Mode
		problem string
		want    Tally
	}{
		{"verified in place", be, File{"a.deb", 5, sum("hello")}, Cheap, "", Tally{Verified: 1}},
		{"missing", be, File{"b.deb", 5, sum("x")}, Cheap, "missing", Tally{}},
		{"wrong size", be, File{"a.deb", 6, sum("hello")}, Cheap, "size 5 does not match the 6 the index records", Tally{Verified: 1}},
		{"wrong content", be, File{"a.deb", 5, sum("other")}, Cheap, "content checksum", Tally{}},
		{"no checksum recorded", be, File{"a.deb", 5, ""}, Content, "", Tally{Unconfirmed: 1}},
		{"cheap mode cannot hash", blind{be}, File{"a.deb", 5, sum("hello")}, Cheap, "", Tally{Unconfirmed: 1}},
		{"content mode downloads", blind{be}, File{"a.deb", 5, sum("hello")}, Content, "", Tally{Verified: 1, Downloaded: 1, BytesRead: 5}},
		{"download finds a different file", blind{be}, File{"a.deb", 5, sum("other")}, Content, "content checksum", Tally{Downloaded: 1, BytesRead: 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := false
			details, tally := Check(ctx, tc.be, tc.file, tc.mode, words, func() *progress.Item { asked = true; return nil })
			got := strings.Join(details, "; ")
			if tc.problem == "" && got != "" || !strings.Contains(got, tc.problem) {
				t.Errorf("details = %q, want %q", got, tc.problem)
			}
			if tally != tc.want {
				t.Errorf("tally = %+v, want %+v", tally, tc.want)
			}
			if asked != (tc.want.Downloaded > 0) {
				t.Errorf("progress item requested = %v, but downloaded = %d", asked, tc.want.Downloaded)
			}
		})
	}
}
