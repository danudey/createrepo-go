package repo

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/repodata"
	"github.com/danudey/createrepo-go/pkg/rpmmeta"
)

// RefreshOptions configures RefreshFromPackages.
type RefreshOptions struct {
	// Progress, if set, is called for each package once it has been re-read.
	// changed reports whether the file's checksum differed from the metadata's.
	Progress func(p *repodata.Package, changed bool)
}

// RefreshResult summarizes what re-reading the packages found.
type RefreshResult struct {
	// Read counts the packages successfully re-read from their files.
	Read int
	// Changed counts those whose file no longer matched the metadata, i.e. the
	// entries this refresh corrected.
	Changed int
	// Downloaded and BytesRead count packages fetched because the backend does
	// not keep its objects as local files.
	Downloaded int
	BytesRead  int64
	// Missing lists locations the metadata references but the backend does not
	// hold. Their index entries are left untouched, so a later verify still
	// reports them rather than the refresh quietly dropping packages.
	Missing []string
	// Unreadable lists locations that exist but could not be parsed as RPMs,
	// with the reason. Their entries are likewise left alone.
	Unreadable []string
	// Duplicates counts metadata records dropped because another record for
	// the very same file already described it. Published metadata should never
	// contain two entries for one href, but a bad publish upstream can leave a
	// stale record beside the current one; re-reading the file collapses them.
	Duplicates int
	// Conflicts lists pairs of distinct locations found to hold byte-identical
	// packages. A repository index is keyed by package checksum and so cannot
	// hold both, but dropping either would leave its RPM unreferenced and so
	// due for deletion. Both entries are therefore left exactly as published
	// and the operator is told, rather than a rebuild quietly deleting a file.
	Conflicts []string
}

// RefreshFromPackages re-reads every RPM the metadata references and replaces
// its index entry with metadata derived from the file itself. It is how a
// repository whose package files were replaced underneath it — a rebuilt
// package published under the same name, say — gets metadata that matches what
// is actually stored: checksum, size, timestamps, dependencies and all.
//
// Packages keep the location they already have; nothing is uploaded, moved or
// deleted. On a backend whose objects are local files each RPM is read in
// place; otherwise every package is downloaded to a temporary file (one at a
// time) and removed again. The caller publishes the corrected metadata with
// Commit.
func (r *Repo) RefreshFromPackages(ctx context.Context, opt RefreshOptions) (*RefreshResult, error) {
	res := &RefreshResult{}

	for _, old := range r.idx.Packages() {
		local, cleanup, size, err := r.readablePath(ctx, old.Location)
		if err == backend.ErrNotExist {
			res.Missing = append(res.Missing, old.Location)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", old.Location, err)
		}
		if size > 0 {
			res.Downloaded++
			res.BytesRead += size
		}

		fresh, err := rpmmeta.FromFile(local, rpmmeta.Options{
			ChecksumType:   r.opt.checksumType(),
			ChangelogLimit: r.opt.ChangelogLimit,
		})
		cleanup()
		if err != nil {
			res.Unreadable = append(res.Unreadable, fmt.Sprintf("%s: %v", old.Location, err))
			continue
		}

		// The file keeps the place the metadata already gave it; only what the
		// metadata says *about* it is refreshed.
		fresh.Location = old.Location
		fresh.XMLBase = old.XMLBase

		// The index is keyed by package checksum, so a refreshed entry can
		// collide with one that is already there.
		if other := r.idx.ByPkgID(fresh.PkgID); other != nil && other.PkgID != old.PkgID {
			if other.Location == fresh.Location {
				// Two records for one file: the stale one goes.
				r.idx.RemoveByPkgID(old.PkgID)
				res.Read++
				res.Duplicates++
				continue
			}
			// Two files, identical content. Keeping only one would orphan the
			// other's RPM and Commit would delete it, so change nothing here.
			res.Conflicts = append(res.Conflicts,
				fmt.Sprintf("%s and %s hold identical packages", other.Location, fresh.Location))
			continue
		}

		changed := fresh.PkgID != old.PkgID
		r.idx.RemoveByPkgID(old.PkgID)
		r.idx.Add(fresh)
		res.Read++
		if changed {
			res.Changed++
		}
		if opt.Progress != nil {
			opt.Progress(fresh, changed)
		}
	}

	sort.Strings(res.Missing)
	sort.Strings(res.Unreadable)
	sort.Strings(res.Conflicts)
	return res, nil
}

// readablePath yields a filesystem path for an object plus a cleanup to call
// when done with it. A backend that stores plain files hands back the file
// itself and a no-op cleanup, reporting size 0 because nothing was
// transferred; any other backend downloads to a temporary file and reports how
// many bytes that cost.
func (r *Repo) readablePath(ctx context.Context, href string) (path string, cleanup func(), transferred int64, err error) {
	noop := func() {}
	if p, ok := backend.LocalPath(r.be, href); ok {
		if _, err := os.Stat(p); err != nil {
			if os.IsNotExist(err) {
				return "", noop, 0, backend.ErrNotExist
			}
			return "", noop, 0, err
		}
		return p, noop, 0, nil
	}

	tmp, err := r.GetToFile(ctx, href)
	if err != nil {
		return "", noop, 0, err
	}
	fi, err := os.Stat(tmp)
	if err != nil {
		os.Remove(tmp)
		return "", noop, 0, err
	}
	return tmp, func() { os.Remove(tmp) }, fi.Size(), nil
}

// RefreshEstimate reports how many packages RefreshFromPackages would have to
// download, and how many bytes that is according to the metadata. Both are zero
// when the backend keeps its objects as local files and the refresh is free.
func (r *Repo) RefreshEstimate() (packages int, bytes int64) {
	if backend.IsLocal(r.be) {
		return 0, 0
	}
	for _, p := range r.idx.Packages() {
		packages++
		bytes += p.SizePackage
	}
	return packages, bytes
}
