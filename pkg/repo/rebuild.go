package repo

import (
	"context"
	"io"
	"os"
	"path"
	"strings"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/repodata"
	"github.com/danudey/createrepo-go/pkg/rpmmeta"
)

// PruneReport summarizes a dependency-aware prune. Removed lists the versions
// actually dropped from the index. When a superseded version is kept because
// another package still depends on that specific version, the dependency is
// recorded in Kept (and the version is not removed). When PruneBreakDeps permits
// dropping such versions anyway, the now-broken dependencies are recorded in
// Broken instead.
type PruneReport struct {
	Removed []*repodata.Package
	Kept    []repodata.Breakage
	Broken  []repodata.Breakage
}

// PruneOlderVersions drops packages superseded by a newer version of the same
// name+arch, keeping only the highest epoch:version-release in each group. A
// superseded version that another package still depends on specifically is kept
// (and reported in PruneReport.Kept) unless PruneBreakDeps is set, in which case
// it is dropped and the broken dependency is reported instead. The RPM blobs of
// dropped versions are garbage-collected on the next Commit.
func (r *Repo) PruneOlderVersions() PruneReport {
	groups := map[string][]*repodata.Package{}
	for _, p := range r.idx.Packages() {
		key := p.Name + "\x00" + p.Arch
		groups[key] = append(groups[key], p)
	}
	var candidates []*repodata.Package
	for _, g := range groups {
		if len(g) < 2 {
			continue
		}
		best := g[0]
		for _, p := range g[1:] {
			if rpmEVRLess(best, p) {
				best = p
			}
		}
		for _, p := range g {
			if p != best {
				candidates = append(candidates, p)
			}
		}
	}
	if len(candidates) == 0 {
		return PruneReport{}
	}

	breakages := repodata.RemovalBreakages(r.idx.Packages(), candidates)
	if r.opt.PruneBreakDeps {
		for _, p := range candidates {
			r.idx.RemoveByPkgID(p.PkgID)
		}
		return PruneReport{Removed: candidates, Broken: breakages}
	}

	protected := make(map[string]bool)
	for _, b := range breakages {
		protected[b.Provider.PkgID] = true
	}
	var removed []*repodata.Package
	for _, p := range candidates {
		if protected[p.PkgID] {
			continue
		}
		r.idx.RemoveByPkgID(p.PkgID)
		removed = append(removed, p)
	}
	return PruneReport{Removed: removed, Kept: breakages}
}

// RelocateAll rewrites every package's location so its RPM lives under prefix
// (keeping the file's basename), returning the number of packages whose
// location changed. A package already at the desired location is left untouched,
// so calling this repeatedly is idempotent. Commit performs the actual moves
// server-side (no re-upload) where the backend supports it.
func (r *Repo) RelocateAll(prefix string) int {
	prefix = strings.Trim(prefix, "/")
	moved := 0
	for _, p := range r.idx.Packages() {
		base := p.Location
		if i := strings.LastIndexByte(base, '/'); i >= 0 {
			base = base[i+1:]
		}
		want := base
		if prefix != "" {
			want = prefix + "/" + base
		}
		if want != p.Location {
			p.Location = want
			moved++
		}
	}
	return moved
}

// ReplaceFromFile parses a (typically re-signed) RPM from localPath, forces its
// location to the given href, replaces any package with the same NEVRA already
// in the index, and stages the file for upload. It is the rebuild counterpart of
// AddRPM used when re-signing existing packages: FromFile recomputes the pkgid
// and size from the new file contents, so the regenerated metadata reflects the
// re-signed RPM.
func (r *Repo) ReplaceFromFile(localPath, location string) (*repodata.Package, error) {
	pkg, err := rpmmeta.FromFile(localPath, rpmmeta.Options{
		ChecksumType:   r.opt.checksumType(),
		ChangelogLimit: r.opt.ChangelogLimit,
	})
	if err != nil {
		return nil, err
	}
	pkg.Location = location
	for _, existing := range r.idx.FindByNEVRA(pkg.Name, pkg.Arch, "") {
		if existing.EVR() == pkg.EVR() {
			r.idx.RemoveByPkgID(existing.PkgID)
		}
	}
	r.idx.Add(pkg)
	r.pending[location] = localPath
	return pkg, nil
}

// TotalSize reports the repository's total stored size and object count. When
// the backend can enumerate its objects (a Lister) the figures cover every file
// and listed is true. Otherwise it falls back to the sizes recorded in the
// metadata (package sizes plus the repodata files repomd.xml references) and
// listed is false, signalling an approximation.
func (r *Repo) TotalSize(ctx context.Context) (bytes int64, files int, listed bool, err error) {
	if l, ok := r.be.(backend.Lister); ok {
		objs, err := l.List(ctx, "")
		if err != nil {
			return 0, 0, false, err
		}
		for _, o := range objs {
			bytes += o.Size
			files++
		}
		return bytes, files, true, nil
	}
	for _, p := range r.idx.Packages() {
		bytes += p.SizePackage
		files++
	}
	if r.old != nil {
		for _, d := range r.old.Data {
			bytes += d.Size
			files++
		}
	}
	return bytes, files, false, nil
}

// ResignTargets returns the packages currently in the index in publish order,
// for a caller that re-signs each one. It is a thin wrapper over the index so
// the CLI does not depend on repodata directly.
func (r *Repo) ResignTargets() []*repodata.Package {
	return r.idx.Packages()
}

// GetToFile streams the object at href from the backend into a new temporary
// file and returns its path. The caller owns the file and must remove it. It is
// used by the re-sign path, the only operation that must download existing RPMs.
func (r *Repo) GetToFile(ctx context.Context, href string) (string, error) {
	rc, err := r.be.Get(ctx, href)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	f, err := os.CreateTemp("", "cr-resign-*-"+tempSuffix(href))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// SetForce toggles whether Commit overwrites already-present files whose content
// differs from what is staged. Re-signing rewrites each RPM in place (same
// location, new content), so the rebuild re-sign path sets this.
func (r *Repo) SetForce(v bool) { r.opt.Force = v }

// Plan computes the commit Plan without transferring or deleting anything,
// regardless of the DryRun option. It is used to preview a rebuild (including
// the directory-scan cleanups) before asking the operator to confirm.
func (r *Repo) Plan(ctx context.Context) (*Plan, error) {
	saved := r.opt.DryRun
	r.opt.DryRun = true
	defer func() { r.opt.DryRun = saved }()
	return r.Commit(ctx)
}

// isRPM reports whether href names an RPM file.
func isRPM(href string) bool {
	return strings.EqualFold(path.Ext(href), ".rpm")
}
