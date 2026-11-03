package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/danudey/createrepo-go/pkg/apt/aptdata"
	"github.com/danudey/createrepo-go/pkg/apt/debmeta"
	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/progress"
	"github.com/danudey/createrepo-go/pkg/xfer"
)

// ConfigPath is the repo-root-relative name of the createapt-go config file.
// It is duplicated from the repoconfig package (rather than imported) so the
// copy engine can treat it as just another object to transfer without repo
// depending on the config layer.
const ConfigPath = "createapt-go.json"

// Release returns the suite's Release as it was published, or nil for a suite
// that does not exist yet.
func (r *Repo) Release() *aptdata.Release { return r.old }

// ReleaseSignatures fetches the suite's InRelease and Release.gpg, returning
// nil for each that is absent. They are fetched on demand so ordinary
// operations do not pay for the extra round trips.
func (r *Repo) ReleaseSignatures(ctx context.Context) (inRelease, detached []byte, err error) {
	suite := r.opt.suite()
	for _, spec := range []struct {
		path string
		into *[]byte
	}{
		{aptdata.InReleasePath(suite), &inRelease},
		{aptdata.ReleaseGPGPath(suite), &detached},
	} {
		data, err := r.getAll(ctx, spec.path)
		if errors.Is(err, backend.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		*spec.into = data
	}
	return inRelease, detached, nil
}

// RawRelease returns the suite's Release file exactly as published. The bytes
// matter because a detached signature is only valid against the original
// document, not a re-rendered one.
func (r *Repo) RawRelease(ctx context.Context) ([]byte, error) {
	return r.getAll(ctx, aptdata.ReleasePath(r.opt.suite()))
}

// IsSigned reports whether the loaded suite carries a Release signature.
func (r *Repo) IsSigned(ctx context.Context) (bool, error) {
	in, det, err := r.ReleaseSignatures(ctx)
	if err != nil {
		return false, err
	}
	return in != nil || det != nil, nil
}

// ClearIndex drops every entry from the in-memory index while keeping the
// record of what the published indexes referenced, so that files the new
// indexes no longer mention are garbage-collected on Commit. It is used by
// `copy --overwrite`, which replaces a destination suite wholesale.
func (r *Repo) ClearIndex() {
	for _, e := range r.idx.Entries() {
		r.idx.RemoveByID(e.ID())
	}
}

// ObjectKind labels a repository file by the role it plays.
type ObjectKind = xfer.Kind

// The kinds of file a repository is made of. Copying treats each kind
// differently: the Release and its signatures are written last, package files
// may be skipped when already present, and anything "other" is carried across
// as-is.
const (
	ObjectRelease    ObjectKind = "release"   // dists/<suite>/Release
	ObjectReleaseSig ObjectKind = "signature" // InRelease, Release.gpg
	ObjectIndex      ObjectKind = "index"     // a Packages/Sources file the Release covers
	ObjectPackage    ObjectKind = "package"   // a pool file an index references
	ObjectConfig     ObjectKind = "config"    // createapt-go.json
	ObjectOther                 = xfer.KindOther
)

// SourceObject is one file that makes up a repository, together with whatever
// integrity data the metadata records for it. Checksum is empty for files
// nothing vouches for (the Release itself, and any stray file).
type SourceObject = xfer.Object

// volatileKind reports the kinds whose content changes without their length
// changing — a Release differs from a previous generation only in its Date and
// checksums — so a copy always rewrites them rather than compare sizes.
func volatileKind(k ObjectKind) bool {
	return k == ObjectRelease || k == ObjectReleaseSig || k == ObjectConfig
}

// Objects enumerates every file that makes up the suite and the pool it draws
// on. When the backend can list its contents, the listing is authoritative and
// files the metadata does not mention are included as ObjectOther; listed is
// then true. Otherwise (a read-only HTTP source, say) the set is derived from
// the metadata alone and listed is false, meaning any file outside the metadata
// is invisible.
func (r *Repo) Objects(ctx context.Context) (objs []SourceObject, listed bool, err error) {
	if r.old == nil {
		return nil, false, fmt.Errorf("no repository metadata loaded")
	}
	suite := r.opt.suite()
	suiteDir := aptdata.SuiteDir(suite)

	known := map[string]SourceObject{}
	add := func(o SourceObject) { known[o.Path] = o }

	add(SourceObject{Path: aptdata.ReleasePath(suite), Kind: ObjectRelease, Size: -1})
	for _, f := range r.old.Files {
		dest := path.Join(suiteDir, f.Path)
		sum := f.Hashes[aptdata.HashSHA256]
		add(SourceObject{Path: dest, Kind: ObjectIndex, Size: f.Size, Checksum: sum})

		// The by-hash copies are not themselves listed in the Release, but
		// their paths follow from the checksums that are, so they can be
		// enumerated even from a source that cannot list its contents. They
		// carry the same bytes as the index they duplicate.
		if r.old.AcquireByHash() {
			for algo, digest := range f.Hashes {
				add(SourceObject{
					Path: aptdata.ByHashPath(path.Dir(dest), algo, digest),
					Kind: ObjectIndex, Size: f.Size, Checksum: sum,
				})
			}
		}
	}
	for _, e := range r.idx.Entries() {
		for _, f := range entryFiles(e) {
			add(SourceObject{Path: f.location, Kind: ObjectPackage, Size: f.size, Checksum: f.sha256})
		}
	}

	if l, ok := r.be.(backend.Lister); ok {
		all, err := l.List(ctx, "")
		if err != nil {
			return nil, false, fmt.Errorf("list %s: %w", r.be, err)
		}
		for _, o := range all {
			// A listing covers the whole repository, including other suites.
			// Only this suite's dists/ tree belongs to this copy; the pool is
			// shared, so pool files are kept only when an index names them.
			if strings.HasPrefix(o.Path, aptdata.DistsDir+"/") && !strings.HasPrefix(o.Path, suiteDir+"/") {
				continue
			}
			if k, ok := known[o.Path]; ok {
				if k.Size < 0 {
					k.Size = o.Size
					known[o.Path] = k
				}
				continue
			}
			if strings.HasPrefix(o.Path, aptdata.PoolDir+"/") {
				// An unreferenced pool file may belong to another suite that
				// shares the pool, so it is not this suite's to copy.
				continue
			}
			known[o.Path] = SourceObject{Path: o.Path, Kind: classifyObject(o.Path, suite), Size: o.Size}
		}
		listed = true
	} else {
		// Without a listing, the files nothing names have to be probed for.
		for _, probe := range []struct {
			path string
			kind ObjectKind
		}{
			{aptdata.InReleasePath(suite), ObjectReleaseSig},
			{aptdata.ReleaseGPGPath(suite), ObjectReleaseSig},
			{ConfigPath, ObjectConfig},
		} {
			fi, err := r.be.Stat(ctx, probe.path)
			if errors.Is(err, backend.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, false, fmt.Errorf("stat %s: %w", probe.path, err)
			}
			add(SourceObject{Path: probe.path, Kind: probe.kind, Size: fi.Size})
		}
	}

	objs = make([]SourceObject, 0, len(known))
	for _, o := range known {
		o.Volatile = volatileKind(o.Kind)
		objs = append(objs, o)
	}
	xfer.Sort(objs, publishRank)
	return objs, listed, nil
}

// classifyObject labels a file found by listing but not named by the metadata.
func classifyObject(p, suite string) ObjectKind {
	switch p {
	case aptdata.ReleasePath(suite):
		return ObjectRelease
	case aptdata.InReleasePath(suite), aptdata.ReleaseGPGPath(suite):
		return ObjectReleaseSig
	case ConfigPath:
		return ObjectConfig
	}
	if strings.HasPrefix(p, aptdata.PoolDir+"/") && isPackageFile(p) {
		return ObjectPackage
	}
	if strings.HasPrefix(p, aptdata.SuiteDir(suite)+"/") {
		return ObjectIndex
	}
	return ObjectOther
}

// publishRank orders a copy: everything a client might need first, then the
// Release (the atomic publish point), then its signatures.
func publishRank(o SourceObject) int {
	switch o.Kind {
	case ObjectRelease:
		return 1
	case ObjectReleaseSig:
		return 2
	default:
		return 0
	}
}

// CopyOptions controls a verbatim repository copy.
type CopyOptions = xfer.CopyOptions

// CopyStats summarizes what a copy transferred.
type CopyStats = xfer.CopyStats

// CopyExact replicates objs from src to dst byte for byte, verifying every
// object whose checksum the metadata records. An object already present at the
// destination with matching content is skipped, which is what makes an
// interrupted copy resumable. Objects are written in the order given, so the
// Release lands last and the destination is never a torn repository.
func CopyExact(ctx context.Context, src, dst backend.Backend, objs []SourceObject, opt CopyOptions) (*CopyStats, error) {
	if opt.TempPrefix == "" {
		opt.TempPrefix = "createapt-copy"
	}
	return xfer.CopyExact(ctx, src, dst, objs, opt)
}

// EntryCopyOptions controls CopyEntriesFrom, which brings selected packages
// from another repository into r and regenerates r's indexes for them.
type EntryCopyOptions struct {
	// Relocate recomputes every copied file's pool location from the
	// destination's layout options rather than keeping the source's.
	Relocate bool

	// Component overrides the component copied entries are indexed in. Empty
	// keeps each entry's component from the source.
	Component string

	// RebuildMetadata re-derives each binary package's index stanza from the
	// .deb itself rather than carrying the source's record over. It requires
	// downloading every package, including ones already present at the
	// destination.
	RebuildMetadata bool

	// Inspect is called with each downloaded file once its checksum has been
	// validated and before it is uploaded. Returning an error aborts the copy.
	Inspect func(e aptdata.Entry, localPath string) error

	// Progress reports each file as it is handled. action is "copy" or "skip".
	Progress func(action string, e aptdata.Entry, location string)

	// Tracker, if set, draws progress bars for the transfers. Each file is moved
	// twice — downloaded, then uploaded — and the two passes appear as separate
	// phases of the same task.
	Tracker *progress.Tracker
}

// CopyEntriesFrom transfers entries (records from another repository's indexes)
// out of src and into r, one file at a time: each is streamed to a temporary
// file, checksum-verified, optionally inspected, uploaded, and then removed, so
// a copy of any size needs room for only one package on local disk.
//
// The entries are added to r's index; the caller publishes them with Commit. A
// file already present at the destination with matching content is not
// transferred again, which is what makes an interrupted copy resumable.
func (r *Repo) CopyEntriesFrom(ctx context.Context, src backend.Backend, entries []aptdata.Entry, opt EntryCopyOptions) (*CopyStats, error) {
	stats := &CopyStats{}
	report := opt.Progress
	if report == nil {
		report = func(string, aptdata.Entry, string) {}
	}

	// The locations come from the source's indexes, so every one is checked
	// before anything moves. A destination location is either one of these or
	// derived from the layout rules, which only produce safe paths.
	for _, e := range entries {
		for _, f := range entryFiles(e) {
			if err := backend.CheckPath(f.location); err != nil {
				return nil, fmt.Errorf("%s: %w", e.ID3(), err)
			}
		}
	}

	if !r.opt.DryRun {
		opt.Tracker.Begin("copy", entryFileCount(entries), 2*entryTotalBytes(entries))
		defer opt.Tracker.End()
	}

	for _, e := range entries {
		indexed, moves, err := r.placeCopy(e, opt)
		if err != nil {
			return nil, err
		}
		// A relocated destination is built from the entry's package name,
		// which comes from the source's indexes too.
		for _, m := range moves {
			if err := backend.CheckPath(m.to); err != nil {
				return nil, fmt.Errorf("%s: %w", e.ID3(), err)
			}
		}

		for _, m := range moves {
			if err := r.copyOneFile(ctx, src, e, m, opt, stats, report); err != nil {
				return nil, err
			}
		}

		// A binary package whose stanza is rebuilt is re-derived from the file
		// that was just written, so the destination's index describes what it
		// actually holds rather than what the source claimed.
		if opt.RebuildMetadata && !r.opt.DryRun {
			if p, ok := indexed.(*aptdata.Package); ok {
				if err := r.reindexFromBackend(ctx, p, opt.Tracker); err != nil {
					return nil, fmt.Errorf("re-index %s: %w", e.ID3(), err)
				}
			}
		}
	}
	return stats, nil
}

// fileMove is one file of a copied entry: where it comes from and where it goes.
type fileMove struct {
	from     string
	to       string
	size     int64
	checksum string
}

// entryFileCount counts the individual files the entries own.
func entryFileCount(entries []aptdata.Entry) int {
	var n int
	for _, e := range entries {
		n += len(e.Locations())
	}
	return n
}

// entryTotalBytes sums the sizes the indexes record for the entries' files.
func entryTotalBytes(entries []aptdata.Entry) int64 {
	var total int64
	for _, e := range entries {
		total += e.TotalBytes()
	}
	return total
}

// copyOneFile transfers a single file of a copied entry.
func (r *Repo) copyOneFile(ctx context.Context, src backend.Backend, e aptdata.Entry, m fileMove, opt EntryCopyOptions, stats *CopyStats, report func(string, aptdata.Entry, string)) error {
	if !opt.RebuildMetadata {
		present, err := xfer.DestinationMatches(ctx, r.be, SourceObject{
			Path: m.to, Kind: ObjectPackage, Size: m.size, Checksum: m.checksum,
		})
		if err != nil {
			return err
		}
		if present {
			r.copied[m.to] = m.size
			stats.Skipped++
			opt.Tracker.Skip(1, 2*max(m.size, 0))
			report("skip", e, m.to)
			return nil
		}
	}
	// A dry run reports the transfer without performing it. The source's own
	// record stands in for the one a rebuild would derive from the file, which
	// is enough to plan against.
	if r.opt.DryRun {
		r.copied[m.to] = m.size
		stats.Copied++
		stats.Bytes += m.size
		report("copy", e, m.to)
		return nil
	}

	task := opt.Tracker.Item(path.Base(m.to), m.size)
	defer task.Done()

	task.Phase(xfer.PhaseDownload)
	local, size, err := xfer.FetchToTemp(ctx, src, SourceObject{
		Path: m.from, Kind: ObjectPackage, Size: m.size, Checksum: m.checksum,
	}, "createapt-copy", task)
	if err != nil {
		return err
	}
	// Every temporary file is released before the next one is fetched, so disk
	// use stays bounded by the largest single file.
	defer os.Remove(local)

	if opt.Inspect != nil {
		if err := opt.Inspect(e, local); err != nil {
			return err
		}
	}
	stats.Copied++
	stats.Bytes += size
	report("copy", e, m.to)

	task.Phase(xfer.PhaseUpload)
	if err := xfer.PutFile(ctx, r.be, m.to, local, task); err != nil {
		return fmt.Errorf("write %s: %w", m.to, err)
	}
	r.copied[m.to] = size
	return nil
}

// placeCopy inserts a copy of a source entry into r's index at its destination
// location, and returns the file moves that copy implies.
func (r *Repo) placeCopy(e aptdata.Entry, opt EntryCopyOptions) (aptdata.Entry, []fileMove, error) {
	component := opt.Component
	if component == "" {
		component = e.ComponentName()
	}
	if component == "" {
		component = r.opt.component()
	}

	switch t := e.(type) {
	case *aptdata.Package:
		cp := &aptdata.Package{Paragraph: t.Clone(), Component: component}
		from := t.Location()
		to := from
		if opt.Relocate || opt.Component != "" {
			if err := aptdata.ValidPackageName(cp.SourceName()); err != nil {
				return nil, nil, fmt.Errorf("%s: source: %w", e.ID3(), err)
			}
			to = path.Join(r.poolDir(component, cp.SourceName()), path.Base(from))
		}
		cp.SetLocation(to)
		r.supersedeCopy(cp)
		r.idx.Add(cp)
		return cp, []fileMove{{from: from, to: to, size: t.Size(), checksum: t.ID()}}, nil

	case *aptdata.Source:
		cp := &aptdata.Source{Paragraph: t.Clone(), Component: component}
		fromDir := t.Directory()
		toDir := fromDir
		if opt.Relocate || opt.Component != "" {
			if err := aptdata.ValidPackageName(cp.Name()); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", e.ID3(), err)
			}
			toDir = r.poolDir(component, cp.Name())
		}
		cp.SetDirectory(toDir)
		var moves []fileMove
		for _, f := range t.SourceFiles() {
			moves = append(moves, fileMove{
				from: f.Location(fromDir), to: f.Location(toDir),
				size: f.Size, checksum: f.SHA256,
			})
		}
		r.supersedeCopy(cp)
		r.idx.AddSource(cp)
		return cp, moves, nil

	default:
		return nil, nil, fmt.Errorf("cannot copy entry of unknown kind %T", e)
	}
}

// reindexFromBackend re-derives a copied binary package's stanza from the file
// now at the destination, replacing the record carried over from the source.
func (r *Repo) reindexFromBackend(ctx context.Context, p *aptdata.Package, prog *progress.Tracker) error {
	loc := p.Location()
	// The re-read is not part of the announced transfer, so it is shown without
	// being counted a second time.
	task := prog.Aside(path.Base(loc), p.Size())
	local, cleanup, err := r.localCopy(ctx, loc, task)
	task.Done()
	if err != nil {
		return err
	}
	defer cleanup()

	fresh, err := debmeta.PackageFromFile(local, debmeta.Options{
		Hashes:     r.opt.hashes(),
		Component:  p.Component,
		PoolLayout: r.opt.poolLayout(),
	})
	if err != nil {
		return err
	}
	fresh.SetLocation(loc)
	r.idx.RemoveByID(p.ID())
	r.idx.Add(fresh)
	r.copied[loc] = fresh.Size()
	return nil
}

// supersedeCopy drops any entry already in the index with the same name,
// architecture and version, so an incremental copy replaces rather than
// duplicates.
func (r *Repo) supersedeCopy(e aptdata.Entry) {
	for _, existing := range r.idx.Find(e.Name(), e.Arch(), e.Version()) {
		r.idx.RemoveByID(existing.ID())
	}
}

// SameRepository reports whether dst holds a subset of src's entries, i.e.
// whether dst looks like a partial copy of src rather than an unrelated
// repository. An empty destination qualifies. It returns a descriptive error
// naming an entry that does not belong when it does not.
func SameRepository(src, dst *aptdata.Index) error {
	if dst.Total() == 0 {
		return nil
	}
	for _, e := range dst.Entries() {
		if src.EntryByID(e.ID()) == nil {
			return fmt.Errorf("the destination holds %s, which the source repository does not have", e.ID3())
		}
	}
	return nil
}
