// Package repo provides high-level repository management on top of a storage
// backend: it loads existing rpm-md metadata, lets callers add or remove
// packages, and publishes the result while transferring as little data as
// possible. Existing RPMs are never downloaded; only the (small) metadata is
// fetched, regenerated locally, and written back, and an RPM that is already
// present is validated remotely (by checksum) rather than re-uploaded.
package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/progress"
	"github.com/danudey/createrepo-go/pkg/repodata"
	"github.com/danudey/createrepo-go/pkg/rpmmeta"
)

// Signer signs the repomd.xml document, producing an ASCII-armored detached
// signature written alongside it as repomd.xml.asc.
type Signer interface {
	SignDetached(data []byte) ([]byte, error)
}

// Options configures repository operations.
type Options struct {
	ChecksumType   string      // package + metadata checksum; only "sha256" supported
	Compression    Compression // metadata compression; default gzip
	LocationPrefix string      // subdirectory for uploaded RPMs, e.g. "Packages"
	ChangelogLimit int         // most-recent changelogs kept per package (0 = all)

	Create     bool // permit creating a repo when none exists
	DryRun     bool // compute and report the plan without transferring
	Force      bool // overwrite a colliding remote file of differing content
	PruneOlder bool // on add, drop older versions of the same name+arch

	// PruneBreakDeps permits pruning (or superseding) a version even when
	// another package in the repository still depends on that specific version.
	// Without it such a version is kept and a warning is recorded instead.
	PruneBreakDeps bool

	// RemoveUnreferencedRPMs deletes .rpm files present in the backend that the
	// published metadata does not reference (requires a Lister backend).
	RemoveUnreferencedRPMs bool
	// RemoveStaleMetadata deletes files under repodata/ that the new repomd.xml
	// does not reference (requires a Lister backend).
	RemoveStaleMetadata bool

	// Signer, if set, signs repomd.xml.
	Signer Signer

	// Tracker, if set, receives the progress of every transfer this repository
	// makes: the RPMs a commit uploads, and the ones a refresh or a re-sign
	// downloads. A nil Tracker reports nothing, which is the default.
	Tracker *progress.Tracker

	// now overrides the timestamp source (for tests); zero means time.Now.
	now int64
}

func (o *Options) checksumType() string {
	if o.ChecksumType == "" {
		return "sha256"
	}
	return o.ChecksumType
}

// Repo is a loaded repository ready for mutation and publishing.
type Repo struct {
	be  backend.Backend
	opt Options
	idx *repodata.Index
	old *repodata.Repomd // previously published index, for metadata GC
	raw []byte           // repomd.xml exactly as loaded (nil for a new repo)

	// pending tracks local RPM files to upload, keyed by destination href.
	pending map[string]string // href -> local path
	// copied records RPMs written to the backend out of band of the staged
	// upload path: the copy command streams each package straight through
	// rather than holding every temporary file until Commit. Commit treats
	// these hrefs as already satisfied. It maps href to size.
	copied map[string]int64
	// original records the RPM locations referenced by the metadata as loaded
	// (href -> pkgid). Commit diffs this against the final index to find files
	// that are no longer referenced (so they can be relocated or deleted).
	original map[string]string

	// pruneKept and pruneBroken accumulate the dependency breakages seen while
	// AddRPM superseded older versions: kept lists versions retained because a
	// dependent still needs them (the default), and broken lists versions
	// dropped anyway because PruneBreakDeps is set. See PruneWarnings.
	pruneKept   []repodata.Breakage
	pruneBroken []repodata.Breakage
}

// Backend exposes the underlying storage (for diagnostics).
func (r *Repo) Backend() backend.Backend { return r.be }

// Index exposes the in-memory package set.
func (r *Repo) Index() *repodata.Index { return r.idx }

// SetPruneOlder toggles whether AddRPM also drops older versions of the same
// name+arch when a newer one is added.
func (r *Repo) SetPruneOlder(v bool) { r.opt.PruneOlder = v }

const repomdPath = "repodata/repomd.xml"

// Open creates a backend for location and loads any existing metadata. If no
// repository exists and opts.Create is false, Open returns an error.
func Open(ctx context.Context, location string, opts Options) (*Repo, error) {
	be, err := backend.Open(ctx, location)
	if err != nil {
		return nil, err
	}
	return OpenWith(ctx, be, opts)
}

// OpenWith is like Open but uses a caller-supplied backend. It is useful for
// embedding (custom backends) and testing.
func OpenWith(ctx context.Context, be backend.Backend, opts Options) (*Repo, error) {
	r := &Repo{
		be:       be,
		opt:      opts,
		idx:      repodata.NewIndex(),
		pending:  map[string]string{},
		copied:   map[string]int64{},
		original: map[string]string{},
	}
	if err := r.load(ctx); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

// Close releases any backend resources.
func (r *Repo) Close() error {
	if c, ok := r.be.(backend.Closer); ok {
		return c.Close()
	}
	return nil
}

// load fetches and parses existing metadata into the index. A repository with
// no repomd.xml is treated as empty (only allowed when opts.Create is set).
func (r *Repo) load(ctx context.Context) error {
	data, err := r.getAll(ctx, repomdPath)
	if errors.Is(err, backend.ErrNotExist) {
		if !r.opt.Create {
			return fmt.Errorf("no repository at %s (use create to initialize)", r.be)
		}
		return nil
	}
	if err != nil {
		return err
	}
	repomd, err := repodata.ParseRepomd(data)
	if err != nil {
		return err
	}
	r.old = repomd
	r.raw = data

	primary, err := r.loadData(ctx, repomd, "primary")
	if err != nil {
		return err
	}
	pkgs, err := repodata.ParsePrimary(primary)
	if err != nil {
		return err
	}

	var filelists map[string][]repodata.File
	if fl, err := r.loadData(ctx, repomd, "filelists"); err == nil && fl != nil {
		if filelists, err = repodata.ParseFilelists(fl); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	var other map[string][]repodata.Changelog
	if ot, err := r.loadData(ctx, repomd, "other"); err == nil && ot != nil {
		if other, err = repodata.ParseOther(ot); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	r.idx = repodata.Merge(pkgs, filelists, other)
	r.idx.Revision = repomd.Revision

	// Snapshot the RPM locations the published metadata references, so Commit
	// can detect files that mutation leaves unreferenced.
	for _, p := range r.idx.Packages() {
		r.original[p.Location] = p.PkgID
	}
	return nil
}

// loadData fetches and decompresses the metadata file of the given type. A
// missing entry yields (nil, nil) so optional documents are tolerated.
func (r *Repo) loadData(ctx context.Context, repomd *repodata.Repomd, typ string) ([]byte, error) {
	d := repomd.DataEntryByType(typ)
	if d == nil {
		return nil, nil
	}
	raw, err := r.getAll(ctx, d.Location)
	if err != nil {
		return nil, fmt.Errorf("fetch %s metadata %s: %w", typ, d.Location, err)
	}
	return decompress(raw)
}

// MetadataDocument returns the decompressed contents of the metadata document
// of the given type, as repomd.xml references it — "primary", "filelists" and
// "other", but equally the documents this tool does not itself generate
// ("group", "updateinfo", "modules", …). It returns (nil, nil) when the
// repository publishes no document of that type, so a caller can probe for one
// without treating its absence as an error.
func (r *Repo) MetadataDocument(ctx context.Context, typ string) ([]byte, error) {
	if r.old == nil {
		return nil, nil
	}
	return r.loadData(ctx, r.old, typ)
}

// AddRPM parses a local RPM, places it at its destination href and stages it
// for upload. A package with the same name+arch+EVR already present is treated
// as an update (superseded). With PruneOlder, older versions are also removed.
func (r *Repo) AddRPM(localPath string) (*repodata.Package, error) {
	pkg, err := rpmmeta.FromFile(localPath, rpmmeta.Options{
		ChecksumType:   r.opt.checksumType(),
		ChangelogLimit: r.opt.ChangelogLimit,
	})
	if err != nil {
		return nil, err
	}
	if r.opt.LocationPrefix != "" {
		pkg.Location = path.Join(strings.Trim(r.opt.LocationPrefix, "/"), pkg.Location)
	}

	// Supersede any identical NEVRA (a rebuild of the same version, always safe)
	// and collect older versions as prune candidates when --prune-older is set.
	var pruneCandidates []*repodata.Package
	for _, existing := range r.idx.FindByNEVRA(pkg.Name, pkg.Arch, "") {
		switch {
		case existing.EVR() == pkg.EVR():
			r.dropPackage(existing)
		case r.opt.PruneOlder && rpmEVRLess(existing, pkg):
			pruneCandidates = append(pruneCandidates, existing)
		}
	}
	if len(pruneCandidates) > 0 {
		// Evaluate the prune against the package set as it will exist once pkg is
		// added, so a newer version being introduced can satisfy dependents.
		all := append(r.idx.Packages(), pkg)
		breakages := repodata.RemovalBreakages(all, pruneCandidates)
		if r.opt.PruneBreakDeps {
			for _, p := range pruneCandidates {
				r.dropPackage(p)
			}
			r.pruneBroken = append(r.pruneBroken, breakages...)
		} else {
			protected := make(map[string]bool)
			for _, b := range breakages {
				protected[b.Provider.PkgID] = true
			}
			for _, p := range pruneCandidates {
				if !protected[p.PkgID] {
					r.dropPackage(p)
				}
			}
			r.pruneKept = append(r.pruneKept, breakages...)
		}
	}

	r.idx.Add(pkg)
	r.pending[pkg.Location] = localPath
	return pkg, nil
}

// PruneWarnings returns the dependency breakages accumulated by AddRPM's
// prune-older path. kept lists versions retained because a dependent still needs
// that specific version (the default behavior); broken lists versions dropped
// despite a dependent because PruneBreakDeps was set. Both are advisory and
// meant to be surfaced to the operator.
func (r *Repo) PruneWarnings() (kept, broken []repodata.Breakage) {
	return r.pruneKept, r.pruneBroken
}

// CheckDependencies reports intra-repository requirements in the current index
// that no package satisfies (see repodata.CheckDependencies). It is used by the
// verify and rebuild commands to validate that removals and prunes have not
// broken the dependency graph.
func (r *Repo) CheckDependencies() []repodata.DependencyProblem {
	return repodata.CheckDependencies(r.idx.Packages())
}

// Remove drops packages matching name (and optional arch/evr) from the index,
// returning them. Their RPM blobs are garbage-collected on Commit once they are
// no longer referenced by the metadata.
func (r *Repo) Remove(name, arch, evr string) []*repodata.Package {
	return r.idx.Remove(name, arch, evr)
}

func (r *Repo) dropPackage(p *repodata.Package) {
	r.idx.RemoveByPkgID(p.PkgID)
}

// nowUnix returns the configured or current unix time.
func (r *Repo) nowUnix() int64 {
	if r.opt.now != 0 {
		return r.opt.now
	}
	return timeNow()
}

// getAll reads an object fully into memory (metadata files are small).
func (r *Repo) getAll(ctx context.Context, relpath string) ([]byte, error) {
	rc, err := r.be.Get(ctx, relpath)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return readAll(rc)
}

// sha256Hex returns the lowercase hex sha256 of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// rpmEVRLess reports whether package a has a lower epoch:version-release than b.
// Epoch dominates; version and release fall back to rpm version comparison.
func rpmEVRLess(a, b *repodata.Package) bool {
	if a.Epoch != b.Epoch {
		return a.Epoch < b.Epoch
	}
	if c := rpmVerCompare(a.Version, b.Version); c != 0 {
		return c < 0
	}
	return rpmVerCompare(a.Release, b.Release) < 0
}
