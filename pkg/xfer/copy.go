package xfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/progress"
)

// Kind labels a repository file by the role it plays. Each format defines its
// own kinds; KindOther is the one every format shares.
type Kind string

// KindOther is a file the metadata does not name: carried across as-is.
const KindOther Kind = "other"

// Object is one file that makes up a repository, together with whatever
// integrity data the metadata records for it.
type Object struct {
	Path string
	Kind Kind
	// Size is the length the metadata records, or -1 when it is unknown.
	Size int64
	// Checksum is the lowercase hex sha256 the metadata records, or empty when
	// nothing vouches for the file (the top-level index itself, a stray file,
	// or a checksum of another type).
	Checksum string
	// Volatile marks a file whose content can change without its length
	// changing — repomd.xml, a Release, their signatures, the config file. An
	// equal size says nothing about such a file, so it is always rewritten.
	Volatile bool
}

// Verifiable reports whether the object carries a checksum to check.
func (o Object) Verifiable() bool { return o.Checksum != "" }

// Sort puts objects into publish order: ascending rank, then path. A format
// ranks its top-level index (the atomic publish point) above everything it
// references, and that index's signatures above the index, so a copy written in
// this order never exposes a torn repository.
func Sort(objs []Object, rank func(Object) int) {
	sort.Slice(objs, func(i, j int) bool {
		if ri, rj := rank(objs[i]), rank(objs[j]); ri != rj {
			return ri < rj
		}
		return objs[i].Path < objs[j].Path
	})
}

// TotalSize sums the sizes the metadata records. Objects of unknown size
// contribute nothing, so the result is a floor.
func TotalSize(objs []Object) int64 {
	var total int64
	for _, o := range objs {
		total += max(o.Size, 0)
	}
	return total
}

// CopyOptions controls a verbatim repository copy.
type CopyOptions struct {
	// DryRun computes the transfer without writing anything.
	DryRun bool
	// Force overwrites a destination object whose content differs from the
	// source's instead of failing.
	Force bool
	// Prune deletes destination objects the source does not have, making the
	// destination an exact replica. It requires a destination that can list
	// its contents.
	Prune bool
	// Classify labels a destination file Prune is about to delete, for
	// Progress. Nil reports every such file as KindOther.
	Classify func(path string) Kind
	// TempPrefix starts the name of each temporary file, so a leftover one is
	// recognizable. It defaults to "copy".
	TempPrefix string
	// Inspect is called with the local temporary copy of each transferred
	// object once its checksum has been validated and before it is written to
	// the destination. Returning an error aborts the copy. It is where package
	// signature verification hooks in.
	Inspect func(obj Object, localPath string) error
	// Progress reports each object as it is handled. action is one of "copy",
	// "skip" or "delete".
	Progress func(action string, obj Object)
	// Tracker, if set, reports the bytes moving through each transfer.
	Tracker *progress.Tracker
}

// CopyStats summarizes what a copy transferred.
type CopyStats struct {
	Copied  int
	Skipped int
	Bytes   int64
	Deleted []string
}

// CheckPaths refuses a set of objects any of whose paths would be unsafe to
// write (see backend.CheckPath). The paths come from a source repository's
// metadata, so they are checked before anything moves.
func CheckPaths(objs []Object) error {
	for _, o := range objs {
		if err := backend.CheckPath(o.Path); err != nil {
			return fmt.Errorf("the source names an unusable path: %w", err)
		}
	}
	return nil
}

// CopyExact replicates objs from src to dst byte for byte, verifying every
// object whose checksum the metadata records. An object already present at the
// destination with matching content is skipped, which is what makes an
// interrupted copy resumable. Objects are written in the order given (see
// Sort), so the destination is never a torn repository.
func CopyExact(ctx context.Context, src, dst backend.Backend, objs []Object, opt CopyOptions) (*CopyStats, error) {
	if err := CheckPaths(objs); err != nil {
		return nil, err
	}
	stats := &CopyStats{}
	report := opt.Progress
	if report == nil {
		report = func(string, Object) {}
	}
	prefix := opt.TempPrefix
	if prefix == "" {
		prefix = "copy"
	}

	if !opt.DryRun {
		// Every object crosses the wire twice — fetched from the source,
		// written to the destination — so that is what the operation has to
		// move. Objects found to be present already are taken back off the
		// total as they are skipped.
		opt.Tracker.Begin("copy", len(objs), 2*TotalSize(objs))
		defer opt.Tracker.End()
	}

	for _, obj := range objs {
		present, err := DestinationMatches(ctx, dst, obj)
		if err != nil {
			return nil, err
		}
		if present && !opt.Force {
			stats.Skipped++
			opt.Tracker.Skip(1, 2*max(obj.Size, 0))
			report("skip", obj)
			continue
		}
		// A dry run reports the transfer without performing it, so it stays
		// cheap even for a repository of any size.
		if opt.DryRun {
			stats.Copied++
			stats.Bytes += max(obj.Size, 0)
			report("copy", obj)
			continue
		}

		item := opt.Tracker.Item(path.Base(obj.Path), obj.Size)
		item.Phase(PhaseDownload)
		local, size, err := FetchToTemp(ctx, src, obj, prefix, item)
		if err != nil {
			item.Done()
			return nil, err
		}
		if opt.Inspect != nil {
			if err := opt.Inspect(obj, local); err != nil {
				item.Done()
				os.Remove(local)
				return nil, err
			}
		}
		stats.Copied++
		stats.Bytes += size
		report("copy", obj)
		item.Phase(PhaseUpload)
		err = PutFile(ctx, dst, obj.Path, local, item)
		item.Done()
		os.Remove(local)
		if err != nil {
			return nil, fmt.Errorf("write %s: %w", obj.Path, err)
		}
	}

	if opt.Prune {
		deleted, err := pruneExtraneous(ctx, dst, objs, opt)
		if err != nil {
			return nil, err
		}
		stats.Deleted = deleted
	}
	return stats, nil
}

// pruneExtraneous removes destination objects the source does not have.
func pruneExtraneous(ctx context.Context, dst backend.Backend, objs []Object, opt CopyOptions) ([]string, error) {
	lister, ok := dst.(backend.Lister)
	if !ok {
		return nil, fmt.Errorf("destination %s cannot list its contents, so extraneous files cannot be removed", dst)
	}
	keep := make(map[string]bool, len(objs))
	for _, o := range objs {
		keep[o.Path] = true
	}
	all, err := lister.List(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dst, err)
	}
	var deleted []string
	for _, o := range all {
		if !keep[o.Path] {
			deleted = append(deleted, o.Path)
		}
	}
	sort.Strings(deleted)
	if opt.Progress != nil {
		for _, p := range deleted {
			kind := KindOther
			if opt.Classify != nil {
				kind = opt.Classify(p)
			}
			opt.Progress("delete", Object{Path: p, Kind: kind})
		}
	}
	if opt.DryRun {
		return deleted, nil
	}
	for _, p := range deleted {
		if err := dst.Delete(ctx, p); err != nil {
			return nil, fmt.Errorf("delete %s: %w", p, err)
		}
	}
	return deleted, nil
}

// DestinationMatches reports whether dst already holds an object identical to
// the source's. A checksum decides it where one is available (recorded in the
// metadata and computable remotely); otherwise the size has to stand in, except
// for a Volatile object, which is always rewritten.
func DestinationMatches(ctx context.Context, dst backend.Backend, obj Object) (bool, error) {
	fi, err := dst.Stat(ctx, obj.Path)
	if errors.Is(err, backend.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", obj.Path, err)
	}
	if obj.Verifiable() {
		if hasher, ok := dst.(backend.RemoteHasher); ok {
			sum, ok, herr := hasher.Hash(ctx, obj.Path, backend.AlgoSHA256)
			if herr != nil && !errors.Is(herr, backend.ErrNotExist) {
				return false, fmt.Errorf("remote hash %s: %w", obj.Path, herr)
			}
			if ok {
				return strings.EqualFold(sum, obj.Checksum), nil
			}
		}
	}
	if obj.Volatile {
		return false, nil
	}
	// Content-addressed or immutable once written, so an equal length means an
	// equal file.
	return obj.Size >= 0 && fi.Size == obj.Size, nil
}

// FetchToTemp streams obj from be into a temporary file, verifying its size and
// (when the metadata records one) its checksum. The caller owns the returned
// file and must remove it. item, which may be nil, is credited with the bytes
// as they arrive.
func FetchToTemp(ctx context.Context, be backend.Backend, obj Object, prefix string, item *progress.Item) (string, int64, error) {
	rc, err := be.Get(ctx, obj.Path)
	if err != nil {
		return "", 0, fmt.Errorf("read %s: %w", obj.Path, err)
	}
	defer rc.Close()

	f, err := createTemp(prefix, obj.Path)
	if err != nil {
		return "", 0, err
	}
	name := f.Name()
	fail := func(err error) (string, int64, error) {
		f.Close()
		os.Remove(name)
		return "", 0, err
	}

	h := sha256.New()
	size, err := io.Copy(item.Writer(io.MultiWriter(f, h)), rc)
	if err != nil {
		return fail(fmt.Errorf("read %s: %w", obj.Path, err))
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", 0, err
	}
	if obj.Size >= 0 && size != obj.Size {
		os.Remove(name)
		return "", 0, fmt.Errorf("%s: got %d bytes, the metadata says %d", obj.Path, size, obj.Size)
	}
	if obj.Verifiable() {
		if sum := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(sum, obj.Checksum) {
			os.Remove(name)
			return "", 0, fmt.Errorf("%s: checksum %s does not match the %s the metadata records",
				obj.Path, sum, obj.Checksum)
		}
	}
	return name, size, nil
}
