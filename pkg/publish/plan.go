// Package publish plans and performs the file side of publishing a repository:
// which package files to upload, which to skip because they are already in
// place, which to relocate server-side, and which to delete once nothing
// references them — and then the writes, in the order that keeps the
// repository whole at every moment. Both tools publish this way; only the
// metadata they generate differs, and that stays with each format.
package publish

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/danudey/createrepo-go/pkg/backend"
)

// ReasonCopied is the UploadAction reason recorded for a file the copy command
// already transferred directly, outside the staged-upload path. Callers that
// report a plan can recognize it to avoid listing those files twice.
const ReasonCopied = "copied from the source repository"

// UploadAction describes what will happen (or happened) to one package file.
type UploadAction struct {
	Location string // destination path, relative to the repository root
	Local    string // source path
	Size     int64
	Upload   bool   // true if the file is/was transferred
	Reason   string // human-readable explanation
}

// CopyAction describes a server-side relocation: an identical copy already
// present in the repository at From is duplicated to To (instead of
// re-uploading it), and From is removed during garbage collection.
type CopyAction struct {
	From string
	To   string
	Size int64
}

// Expect is what the new metadata records about one file it references.
type Expect struct {
	Size int64
	// SHA256 is the recorded checksum, lowercase hex, or empty when the
	// metadata records none this tool can check.
	SHA256 string
	// ID identifies the content across locations, so a file that moved can be
	// matched with the copy already stored at its old location. Empty means
	// the file cannot be relocated.
	ID string
}

// State is what a planner needs to know about a repository mid-update.
type State struct {
	// Referenced is every file the new metadata references.
	Referenced map[string]Expect
	// Original maps every file the metadata referenced as loaded to its ID.
	Original map[string]string
	// Pending maps each staged upload's destination to its local file.
	Pending map[string]string
	// Copied maps files the copy command already wrote to their sizes.
	Copied map[string]int64
	// Force re-uploads staged files even when an identical copy is present.
	Force bool
	// Describe names a file in an error. Nil uses the path.
	Describe func(path string) string
}

// Files is the file side of a publish plan.
type Files struct {
	Uploads       []UploadAction // files that need transferring
	Skipped       []UploadAction // files already present (validated remotely)
	Copies        []CopyAction   // files relocated server-side (no re-upload)
	Deleted       []string       // files no longer referenced, to delete
	BytesToUpload int64          // bytes to transfer (skips and relocations excluded)
}

// PlanFiles decides, for each staged file, whether it must be uploaded,
// relocated from an existing copy, or skipped, and which now-unreferenced files
// must be garbage-collected. It uses remote validation (a RemoteHasher, or the
// size) to avoid re-uploading files that are already present, and never
// downloads one.
func PlanFiles(ctx context.Context, be backend.Backend, s State) (*Files, error) {
	plan := &Files{}
	describe := s.Describe
	if describe == nil {
		describe = func(p string) string { return p }
	}

	// Orphans: files referenced as loaded but no longer referenced now. They
	// are garbage-collected after the index swap; an orphan whose content
	// matches a file that needs placing can serve as the source of a
	// server-side relocation instead of a re-upload.
	orphanByID := map[string]string{}
	for _, p := range sortedKeys(s.Original) {
		if _, stillReferenced := s.Referenced[p]; stillReferenced {
			continue
		}
		plan.Deleted = append(plan.Deleted, p)
		if id := s.Original[p]; id != "" {
			if _, ok := orphanByID[id]; !ok {
				orphanByID[id] = p
			}
		}
	}

	_, canCopy := be.(backend.Copier)
	hasher, _ := be.(backend.RemoteHasher)

	for _, dest := range sortedKeys(s.Pending) {
		want, known := s.Referenced[dest]
		action := UploadAction{Location: dest, Local: s.Pending[dest], Size: want.Size}

		fi, err := be.Stat(ctx, dest)
		missing := false
		switch {
		case errors.Is(err, backend.ErrNotExist):
			missing = true
			action.Upload = true
			action.Reason = "not present"
		case err != nil:
			return nil, fmt.Errorf("stat %s: %w", dest, err)
		case !known:
			// Shouldn't happen: a staged file is always one the metadata
			// references. Without an expectation nothing can vouch for the
			// copy already there, so it is replaced.
			action.Upload = true
			action.Reason = "no metadata to compare against"
		default:
			action.Upload, action.Reason, err = resolveExisting(ctx, dest, want, fi, hasher, s.Force)
			if err != nil {
				return nil, err
			}
		}

		// If the file is missing but an identical copy already exists in the
		// repository at a now-unreferenced location, relocate it server-side
		// instead of re-uploading. --force always re-uploads from local (it
		// deliberately does not trust the remote copy).
		if missing && !s.Force && canCopy && want.ID != "" {
			if from, ok := orphanByID[want.ID]; ok {
				plan.Copies = append(plan.Copies, CopyAction{From: from, To: dest, Size: want.Size})
				continue
			}
		}

		if action.Upload {
			plan.Uploads = append(plan.Uploads, action)
			plan.BytesToUpload += action.Size
		} else {
			plan.Skipped = append(plan.Skipped, action)
		}
	}

	// Relocated existing files: referenced by the new metadata, not staged as
	// a local upload, and at a location the loaded metadata did not have
	// (rebuild moving files under a new prefix or layout). Their content
	// already lives in the repository at a now-orphaned location, so move it
	// server-side instead of re-uploading. This pass is a no-op for add,
	// remove and create, where every referenced location is either pending or
	// unchanged.
	for _, dest := range sortedKeys(s.Referenced) {
		want := s.Referenced[dest]
		if _, isPending := s.Pending[dest]; isPending {
			continue
		}
		if _, wasOriginal := s.Original[dest]; wasOriginal {
			continue
		}
		// Already transferred out of band by the copy command (or, under
		// --dry-run, accounted for as if it had been).
		if size, wasCopied := s.Copied[dest]; wasCopied {
			plan.Skipped = append(plan.Skipped, UploadAction{Location: dest, Size: size, Reason: ReasonCopied})
			continue
		}
		// A new location for existing content. If it is already present (a
		// resumed rebuild, say), trust it; otherwise relocate from the orphan.
		if _, err := be.Stat(ctx, dest); err == nil {
			plan.Skipped = append(plan.Skipped, UploadAction{Location: dest, Size: want.Size, Reason: "present at new location"})
			continue
		} else if !errors.Is(err, backend.ErrNotExist) {
			return nil, fmt.Errorf("stat %s: %w", dest, err)
		}
		if canCopy && want.ID != "" {
			if from, ok := orphanByID[want.ID]; ok {
				plan.Copies = append(plan.Copies, CopyAction{From: from, To: dest, Size: want.Size})
				continue
			}
		}
		return nil, fmt.Errorf("cannot place %s: no local copy staged and the backend cannot move it server-side", describe(dest))
	}
	return plan, nil
}

// resolveExisting decides whether an already-present file must be re-uploaded.
func resolveExisting(ctx context.Context, dest string, want Expect, fi *backend.FileInfo, hasher backend.RemoteHasher, force bool) (upload bool, reason string, err error) {
	// --force means "upload regardless": overwrite the remote file even if it
	// looks identical. This is the only correct behavior when we cannot prove
	// the remote file matches, or when the local file will differ once written
	// (it was just signed, say).
	if force {
		return true, "overwriting (--force)", nil
	}
	if hasher != nil && want.SHA256 != "" {
		sum, ok, herr := hasher.Hash(ctx, dest, backend.AlgoSHA256)
		if herr != nil && !errors.Is(herr, backend.ErrNotExist) {
			return false, "", fmt.Errorf("remote hash %s: %w", dest, herr)
		}
		if ok {
			if strings.EqualFold(sum, want.SHA256) {
				return false, "present, checksum verified remotely", nil
			}
			return false, "", fmt.Errorf("%s already exists with different content (checksum %s != %s); use --force to overwrite", dest, sum, want.SHA256)
		}
	}
	// Fall back to a size comparison when no checksum is available.
	if fi.Size == want.Size {
		return false, "present, size matches (checksum not verified)", nil
	}
	return false, "", fmt.Errorf("%s already exists with different size (%d != %d); use --force to overwrite", dest, fi.Size, want.Size)
}

// Unreferenced lists the files under prefix that keep rejects and that are not
// already in one of the scheduled lists, in sorted order. It is how a rebuild
// finds package files and metadata the published state no longer mentions.
func Unreferenced(ctx context.Context, lister backend.Lister, prefix string, keep func(path string) bool, scheduled ...[]string) ([]string, error) {
	skip := map[string]bool{}
	for _, list := range scheduled {
		for _, p := range list {
			skip[p] = true
		}
	}
	objs, err := lister.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, o := range objs {
		if keep(o.Path) || skip[o.Path] {
			continue
		}
		out = append(out, o.Path)
	}
	sort.Strings(out)
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
