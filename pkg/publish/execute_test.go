package publish

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/xfer"
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

// A publish that read the index before another publish replaced it must not
// swap its own in: it would silently undo the other publish's changes.
func TestExecuteStopsWhenTheIndexChangedMeanwhile(t *testing.T) {
	ctx := context.Background()
	be, err := backend.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	put := func(p, body string) {
		if err := be.Put(ctx, p, strings.NewReader(body), int64(len(body))); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string {
		data, err := xfer.ReadAll(ctx, be, p)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	put("repomd.xml", "v1")
	steps := Steps{
		Metadata: []File{{Path: "data-2.xml", Data: []byte("data")}},
		Swap:     []File{{Path: "repomd.xml", Data: []byte("v2")}},
		Guard:    []Unchanged{{Path: "repomd.xml", Exists: true, Data: []byte("v1")}},
	}

	put("repomd.xml", "v1-from-someone-else")
	if err := Execute(ctx, be, steps, nil); !errors.Is(err, ErrConcurrentPublish) {
		t.Fatalf("Execute over a changed index = %v, want ErrConcurrentPublish", err)
	}
	if got := read("repomd.xml"); got != "v1-from-someone-else" {
		t.Errorf("repomd.xml = %q; the other publish was overwritten", got)
	}

	// A first publish expects no index at all.
	steps.Guard = []Unchanged{{Path: "repomd.xml"}}
	if err := Execute(ctx, be, steps, nil); !errors.Is(err, ErrConcurrentPublish) {
		t.Errorf("Execute expecting a new repository over an existing one = %v, want ErrConcurrentPublish", err)
	}

	steps.Guard = []Unchanged{{Path: "repomd.xml", Exists: true, Data: []byte("v1-from-someone-else")}}
	if err := Execute(ctx, be, steps, nil); err != nil {
		t.Fatalf("Execute with an up-to-date guard: %v", err)
	}
	if got := read("repomd.xml"); got != "v2" {
		t.Errorf("repomd.xml = %q, want v2", got)
	}
}
