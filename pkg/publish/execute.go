package publish

import (
	"context"
	"fmt"
	"os"
	"path"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/progress"
	"github.com/danudey/createrepo-go/pkg/xfer"
)

// File is one generated document and where it is written.
type File struct {
	Path string
	Data []byte
}

// Steps is everything a publish writes and deletes, grouped by when it may
// happen. Execute performs them in an order that keeps the repository whole at
// every moment: nothing a client can reach names a file that is not yet in
// place, and nothing is deleted while the published index still names it.
type Steps struct {
	// Copies relocate existing files server-side. The new copy is written
	// first; its source is in GC.
	Copies []CopyAction
	// Uploads transfer package files. Only actions with Upload set are sent.
	Uploads []UploadAction
	// Metadata is every file the new index names, written in order — so a
	// caller that writes the same content under two names (an apt by-hash
	// copy and its plain path) lists the one an older index might name first.
	Metadata []File
	// Swap is the top-level index and its signatures: the atomic publish
	// point, written in order after everything it names is in place.
	Swap []File
	// Retract is deleted right after the swap: files that would contradict the
	// new index, such as a signature over the previous one when the new one is
	// unsigned. A missing file is not an error.
	Retract []string
	// GC is deleted last, group by group: superseded metadata, files nothing
	// references any longer, relocation sources.
	GC [][]string
}

// work counts what Execute is about to do, for the overall progress bar: every
// file it writes, relocates or deletes, and the bytes the writes amount to. A
// relocation and a deletion move no bytes through this process, so they count
// as items only.
func (s Steps) work() (items int, bytes int64) {
	items = len(s.Copies)
	for _, a := range s.Uploads {
		items++
		bytes += max(a.Size, 0)
	}
	for _, f := range append(append([]File(nil), s.Metadata...), s.Swap...) {
		items++
		bytes += int64(len(f.Data))
	}
	for _, g := range s.GC {
		items += len(g)
	}
	return items, bytes
}

// Execute performs a publish. tr, which may be nil, reports it as one
// operation covering every write and deletion.
func Execute(ctx context.Context, be backend.Backend, s Steps, tr *progress.Tracker) error {
	items, bytes := s.work()
	tr.Begin("publish", items, bytes)
	defer tr.End()

	// 1. Relocate identical files server-side. The old copy is removed during
	// GC, after the swap, so the repository is never torn.
	for _, c := range s.Copies {
		if err := xfer.ServerCopy(ctx, be, c.From, c.To); err != nil {
			return fmt.Errorf("relocate %s -> %s: %w", c.From, c.To, err)
		}
		tr.Count(1)
	}

	// 2. Upload the package files that need it. They must be in place before
	// any index names them.
	for _, a := range s.Uploads {
		if !a.Upload {
			continue
		}
		if err := putFile(ctx, be, a.Location, a.Local, tr); err != nil {
			return fmt.Errorf("upload %s: %w", a.Location, err)
		}
	}

	// 3. Write the metadata the new index names.
	for _, f := range s.Metadata {
		if err := putBytes(ctx, be, f, tr); err != nil {
			return fmt.Errorf("write %s: %w", f.Path, err)
		}
	}

	// 4. Swap the top-level index last — the atomic publish point.
	for _, f := range s.Swap {
		if err := putBytes(ctx, be, f, tr); err != nil {
			return fmt.Errorf("write %s: %w", f.Path, err)
		}
	}
	for _, p := range s.Retract {
		if err := be.Delete(ctx, p); err != nil {
			return fmt.Errorf("delete stale %s: %w", p, err)
		}
	}

	// 5. Garbage-collect what nothing references any longer, including the
	// sources of the relocations in step 1.
	for _, group := range s.GC {
		for _, p := range group {
			if err := be.Delete(ctx, p); err != nil {
				return fmt.Errorf("delete %s: %w", p, err)
			}
			tr.Count(1)
		}
	}
	return nil
}

func putFile(ctx context.Context, be backend.Backend, dest, local string, tr *progress.Tracker) error {
	fi, err := os.Stat(local)
	if err != nil {
		return err
	}
	item := tr.Item(path.Base(dest), fi.Size())
	defer item.Done()
	return xfer.PutFile(ctx, be, dest, local, item)
}

func putBytes(ctx context.Context, be backend.Backend, f File, tr *progress.Tracker) error {
	item := tr.Item(path.Base(f.Path), int64(len(f.Data)))
	defer item.Done()
	return xfer.PutBytes(ctx, be, f.Path, f.Data, item)
}
