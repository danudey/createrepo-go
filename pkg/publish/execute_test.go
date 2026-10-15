package publish

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/danudey/createrepo-go/pkg/backend"
)

// recorder logs every write and deletion in order, forwarding to a real
// backend so copies have something to copy.
type recorder struct {
	backend.Backend
	mu  sync.Mutex
	log []string
}

func (r *recorder) note(s string) { r.mu.Lock(); r.log = append(r.log, s); r.mu.Unlock() }

func (r *recorder) Put(ctx context.Context, p string, body io.Reader, n int64) error {
	r.note("put " + p)
	return r.Backend.Put(ctx, p, body, n)
}

func (r *recorder) Delete(ctx context.Context, p string) error {
	r.note("delete " + p)
	return r.Backend.Delete(ctx, p)
}

func (r *recorder) Copy(ctx context.Context, from, to string) error {
	r.note("copy " + from + " " + to)
	return r.Backend.(backend.Copier).Copy(ctx, from, to)
}

// The order is the publish's atomicity contract: everything the new index
// names exists before the index is swapped in, and nothing it might still name
// is deleted before then.
func TestExecuteKeepsTheRepositoryWholeAtEveryStep(t *testing.T) {
	inner, _ := repoWith(t, map[string]string{"old/a.deb": "aaa", "meta/old-index": "x", "InRelease": "sig"})
	be := &recorder{Backend: inner}
	local := filepath.Join(t.TempDir(), "b.deb")
	if err := os.WriteFile(local, []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Execute(context.Background(), be, Steps{
		Copies:   []CopyAction{{From: "old/a.deb", To: "pool/a.deb", Size: 3}},
		Uploads:  []UploadAction{{Location: "pool/b.deb", Local: local, Size: 4, Upload: true}},
		Metadata: []File{{Path: "meta/by-hash/1", Data: []byte("i")}, {Path: "meta/index", Data: []byte("i")}},
		Swap:     []File{{Path: "Release", Data: []byte("r")}},
		Retract:  []string{"InRelease"},
		GC:       [][]string{{"meta/old-index"}, {"old/a.deb"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"copy old/a.deb pool/a.deb",
		"put pool/b.deb",
		"put meta/by-hash/1",
		"put meta/index",
		"put Release",
		"delete InRelease",
		"delete meta/old-index",
		"delete old/a.deb",
	}
	if got := strings.Join(be.log, "\n"); got != strings.Join(want, "\n") {
		t.Errorf("publish order:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}
