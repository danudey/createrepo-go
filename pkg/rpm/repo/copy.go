package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/progress"
	"github.com/danudey/createrepo-go/pkg/rpm/repodata"
	"github.com/danudey/createrepo-go/pkg/rpm/rpmmeta"
	"github.com/danudey/createrepo-go/pkg/xfer"
)

// ConfigPath is the repo-root-relative name of the createrepo-go config file.
// It is duplicated from the repoconfig package (rather than imported) so the
// copy engine can treat it as just another object to transfer without repo
// depending on the config layer.
const ConfigPath = "createrepo-go.json"

// Repomd returns the repository index as it was published, or nil for a
// repository that does not exist yet.
func (r *Repo) Repomd() *repodata.Repomd { return r.old }

// RawRepomd returns repomd.xml exactly as it was loaded, or nil for a
// repository that does not exist yet. The bytes matter because a detached
// signature is only valid against the original document, not a re-rendered one.
func (r *Repo) RawRepomd() []byte { return r.raw }

// MetadataSignature fetches repodata/repomd.xml.asc, returning (nil, nil) when
// the repository's metadata is unsigned. It is fetched on demand so ordinary
// operations do not pay for the extra round trip.
func (r *Repo) MetadataSignature(ctx context.Context) ([]byte, error) {
	data, err := r.getAll(ctx, repomdPath+".asc")
	if errors.Is(err, backend.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// ClearIndex drops every package from the in-memory index while keeping the
// record of what the published metadata referenced, so that RPMs the new
// metadata no longer mentions are garbage-collected on Commit. It is used by
// `copy --overwrite`, which replaces a destination repository wholesale, so
// the documents Commit would otherwise carry over (comps, updateinfo, ...) are
// dropped too: they describe the repository being replaced.
func (r *Repo) ClearIndex() {
	for _, p := range r.idx.Packages() {
		r.idx.RemoveByPkgID(p.PkgID)
	}
	r.replaced = true
}

// ObjectKind labels a repository file by the role it plays.
type ObjectKind = xfer.Kind

// The kinds of file a repository is made of. Copying treats each kind
// differently: repomd and its signature are written last, packages may be
// skipped when already present, and anything "other" is carried across as-is.
const (
	ObjectRepomd    ObjectKind = "repomd"    // repodata/repomd.xml
	ObjectRepomdSig ObjectKind = "signature" // repodata/repomd.xml.asc
	ObjectMetadata  ObjectKind = "metadata"  // an index file repomd.xml references
	ObjectPackage   ObjectKind = "package"   // an RPM the metadata references
	ObjectConfig    ObjectKind = "config"    // createrepo-go.json
	ObjectOther                = xfer.KindOther
)

// SourceObject is one file that makes up a repository, together with whatever
// integrity data the metadata records for it. Checksum is set only for a
// sha256 the metadata records, the one type this tool can check.
type SourceObject = xfer.Object

// sha256Only returns sum when the metadata says it is a sha256, and nothing
// otherwise: a checksum of another type cannot be checked, so it must not be
// mistaken for one that can.
func sha256Only(sum, typ string) string {
	if strings.EqualFold(typ, backend.AlgoSHA256) {
		return sum
	}
	return ""
}

// packageObject describes package p as stored at loc.
func packageObject(p *repodata.Package, loc string) SourceObject {
	return SourceObject{
		Path: loc, Kind: ObjectPackage, Size: p.SizePackage,
		Checksum: sha256Only(p.PkgID, p.ChecksumType),
	}
}

// volatileKind reports the kinds whose content changes without their length
// changing — repomd.xml differs from a previous generation only in its
// revision — so a copy always rewrites them rather than compare sizes.
func volatileKind(k ObjectKind) bool {
	return k == ObjectRepomd || k == ObjectRepomdSig || k == ObjectConfig
}

// Objects enumerates every file that makes up the repository. When the backend
// can list its contents, the listing is authoritative and files the metadata
// does not mention are included as ObjectOther; listed is then true. Otherwise
// (a read-only HTTP source, say) the set is derived from the metadata alone and
// listed is false, meaning any file outside the metadata is invisible.
func (r *Repo) Objects(ctx context.Context) (objs []SourceObject, listed bool, err error) {
	known := map[string]SourceObject{}
	add := func(o SourceObject) { known[o.Path] = o }

	if r.old == nil {
		return nil, false, fmt.Errorf("no repository metadata loaded")
	}
	add(SourceObject{Path: repomdPath, Kind: ObjectRepomd, Size: int64(len(r.raw))})
	for _, d := range r.old.Data {
		add(SourceObject{
			Path: d.Location, Kind: ObjectMetadata, Size: d.Size,
			Checksum: sha256Only(d.Checksum, d.ChecksumType),
		})
	}
	for _, p := range r.idx.Packages() {
		add(packageObject(p, p.Location))
	}

	if l, ok := r.be.(backend.Lister); ok {
		all, err := l.List(ctx, "")
		if err != nil {
			return nil, false, fmt.Errorf("list %s: %w", r.be, err)
		}
		for _, o := range all {
			if k, ok := known[o.Path]; ok {
				if k.Size < 0 {
					k.Size = o.Size
					known[o.Path] = k
				}
				continue
			}
			known[o.Path] = SourceObject{Path: o.Path, Kind: classifyObject(o.Path), Size: o.Size}
		}
		listed = true
	} else {
		// Without a listing, the two files that are never named by the metadata
		// have to be probed for individually.
		for _, probe := range []struct {
			path string
			kind ObjectKind
		}{
			{repomdPath + ".asc", ObjectRepomdSig},
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
func classifyObject(p string) ObjectKind {
	switch {
	case p == repomdPath:
		return ObjectRepomd
	case p == repomdPath+".asc":
		return ObjectRepomdSig
	case p == ConfigPath:
		return ObjectConfig
	case isRPM(p):
		return ObjectPackage
	default:
		return ObjectOther
	}
}

// publishRank orders a copy: everything a client might need first, then
// repomd.xml (the atomic publish point), then its signature.
func publishRank(o SourceObject) int {
	switch o.Kind {
	case ObjectRepomd:
		return 1
	case ObjectRepomdSig:
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
// interrupted copy resumable. Objects are written in the order given, so
// repomd.xml lands last and the destination is never a torn repository.
func CopyExact(ctx context.Context, src, dst backend.Backend, objs []SourceObject, opt CopyOptions) (*CopyStats, error) {
	if opt.Classify == nil {
		opt.Classify = classifyObject
	}
	if opt.TempPrefix == "" {
		opt.TempPrefix = "cr-copy"
	}
	return xfer.CopyExact(ctx, src, dst, objs, opt)
}

// PackageCopyOptions controls CopyPackagesFrom, which brings selected packages
// from another repository into r and regenerates r's metadata for them.
type PackageCopyOptions struct {
	// LocationPrefix relocates every copied RPM under this subdirectory.
	// Relocate must be set for it to apply; otherwise each package keeps the
	// location it had in the source repository.
	LocationPrefix string
	Relocate       bool

	// SetXMLBase replaces the <location xml:base> of every copied package with
	// XMLBase (empty removes the attribute). Without it the source's value is
	// carried over, which would leave clients of the copy fetching packages
	// from the original repository.
	SetXMLBase bool
	XMLBase    string

	// RebuildMetadata re-derives each package's metadata from the RPM itself
	// rather than carrying the source's records over. It requires downloading
	// every package, including ones already present at the destination.
	RebuildMetadata bool

	// Sign, when set, signs each package after it is downloaded and before it
	// is uploaded, returning the path of the signed copy plus a cleanup. A
	// signed package's content differs from the source's, so its metadata is
	// always re-derived and the destination copy is always overwritten.
	Sign func(localPath string) (signed string, cleanup func(), err error)

	// Inspect is called with each downloaded RPM once its checksum has been
	// validated and before it is signed or uploaded. Returning an error aborts
	// the copy. It is where package signature verification hooks in.
	Inspect func(p *repodata.Package, localPath string) error

	// Progress reports each package as it is handled. action is "copy" or
	// "skip".
	Progress func(action string, p *repodata.Package, location string)

	// Tracker, if set, reports the bytes moving through each transfer.
	Tracker *progress.Tracker
}

// CopyPackagesFrom transfers pkgs (records from another repository's metadata)
// out of src and into r, one package at a time: each is streamed to a temporary
// file, checksum-verified, optionally inspected and signed, uploaded, and then
// removed, so a copy of any size needs room for only one package on local disk.
//
// The packages are added to r's index; the caller publishes them with Commit.
// A package already present at the destination with matching content is not
// transferred again, which is what makes an interrupted copy resumable.
func (r *Repo) CopyPackagesFrom(ctx context.Context, src backend.Backend, pkgs []*repodata.Package, opt PackageCopyOptions) (*CopyStats, error) {
	stats := &CopyStats{}
	report := opt.Progress
	if report == nil {
		report = func(string, *repodata.Package, string) {}
	}

	// Each package is downloaded and then uploaded again, so it costs twice its
	// size. Packages already at the destination come back off the total.
	var expected int64
	for _, p := range pkgs {
		expected += 2 * max(p.SizePackage, 0)
	}
	// The locations come from the source's metadata, so every one is checked
	// before anything moves.
	for _, p := range pkgs {
		for _, loc := range []string{p.Location, r.copyLocation(p, opt)} {
			if err := backend.CheckPath(loc); err != nil {
				return nil, fmt.Errorf("%s: %w", p.NEVRA(), err)
			}
		}
	}

	if !r.opt.DryRun {
		opt.Tracker.Begin("copy", len(pkgs), expected)
		defer opt.Tracker.End()
	}

	for _, p := range pkgs {
		loc := r.copyLocation(p, opt)
		// Checked before anything is written: the copy uploads each file as
		// it goes, so a second package at loc would overwrite the first.
		if err := r.locationFree(p, loc); err != nil {
			return nil, err
		}
		obj := packageObject(p, p.Location)

		// Signing rewrites the file, so a copy already at the destination is
		// never reusable and the metadata always has to be re-derived.
		rewrites := opt.Sign != nil
		if !rewrites && !opt.RebuildMetadata {
			present, err := xfer.DestinationMatches(ctx, r.be, packageObject(p, loc))
			if err != nil {
				return nil, err
			}
			if present {
				r.addCopied(p, loc, opt)
				stats.Skipped++
				opt.Tracker.Skip(1, 2*max(p.SizePackage, 0))
				report("skip", p, loc)
				continue
			}
		}
		// A dry run reports the transfer without performing it. The source's
		// own metadata record stands in for the one a rebuild would derive from
		// the package, which is enough to plan against.
		if r.opt.DryRun {
			r.addCopied(p, loc, opt)
			r.copied[loc] = p.SizePackage
			stats.Copied++
			stats.Bytes += p.SizePackage
			report("copy", p, loc)
			continue
		}

		item := opt.Tracker.Item(path.Base(p.Location), p.SizePackage)
		item.Phase(xfer.PhaseDownload)
		local, size, err := xfer.FetchToTemp(ctx, src, obj, "cr-copy", item)
		if err != nil {
			item.Done()
			return nil, err
		}
		if opt.Inspect != nil {
			if err := opt.Inspect(p, local); err != nil {
				item.Done()
				os.Remove(local)
				return nil, err
			}
		}

		// Every temporary file this iteration created is released before the
		// next package is fetched, so disk use stays bounded by one package.
		upload, release := local, func() { os.Remove(local) }
		if opt.Sign != nil {
			item.Phase("sign")
			signed, cleanup, err := opt.Sign(local)
			os.Remove(local)
			if err != nil {
				item.Done()
				return nil, fmt.Errorf("sign %s: %w", p.NEVRA(), err)
			}
			upload, release = signed, cleanup
		}

		var indexed *repodata.Package
		if rewrites || opt.RebuildMetadata {
			indexed, err = r.addFromFile(upload, loc, opt)
			if err != nil {
				item.Done()
				release()
				return nil, fmt.Errorf("index %s: %w", p.NEVRA(), err)
			}
		} else {
			indexed = r.addCopied(p, loc, opt)
		}

		stats.Copied++
		stats.Bytes += size
		report("copy", p, loc)

		item.Phase(xfer.PhaseUpload)
		err = xfer.PutFile(ctx, r.be, loc, upload, item)
		item.Done()
		if err != nil {
			release()
			return nil, fmt.Errorf("write %s: %w", loc, err)
		}
		release()
		r.copied[loc] = indexed.SizePackage
	}
	return stats, nil
}

// copyLocation returns the destination href for a copied package.
func (r *Repo) copyLocation(p *repodata.Package, opt PackageCopyOptions) string {
	if !opt.Relocate {
		return p.Location
	}
	base := path.Base(p.Location)
	prefix := strings.Trim(opt.LocationPrefix, "/")
	if prefix == "" {
		return base
	}
	return prefix + "/" + base
}

// addCopied inserts the source's own metadata record for a package into the
// index at its destination location, replacing any package with the same NEVRA
// that the destination already had.
func (r *Repo) addCopied(p *repodata.Package, loc string, opt PackageCopyOptions) *repodata.Package {
	cp := *p
	cp.Location = loc
	if opt.SetXMLBase {
		cp.XMLBase = opt.XMLBase
	}
	r.supersede(&cp)
	r.idx.Add(&cp)
	return &cp
}

// addFromFile re-derives a package's metadata from the RPM at localPath and
// indexes it at loc. It is used when the copy rewrites the package (signing) or
// when the caller asked for the metadata to be rebuilt from the packages.
func (r *Repo) addFromFile(localPath, loc string, opt PackageCopyOptions) (*repodata.Package, error) {
	pkg, err := rpmmeta.FromFile(localPath, r.packageOptions())
	if err != nil {
		return nil, err
	}
	pkg.Location = loc
	if opt.SetXMLBase {
		pkg.XMLBase = opt.XMLBase
	}
	r.supersede(pkg)
	r.idx.Add(pkg)
	return pkg, nil
}

// supersede drops any package already in the index with the same name, arch and
// EVR as p, so an incremental copy replaces rather than duplicates.
func (r *Repo) supersede(p *repodata.Package) {
	for _, existing := range r.idx.FindByNEVRA(p.Name, p.Arch, p.EVR()) {
		r.idx.RemoveByPkgID(existing.PkgID)
	}
}

// SameRepository reports whether dst holds a subset of src's packages, i.e.
// whether dst looks like a partial copy of src rather than an unrelated
// repository. An empty destination qualifies. It returns a descriptive error
// naming a package that does not belong when it does not.
func SameRepository(src, dst *repodata.Index) error {
	if dst.Len() == 0 {
		return nil
	}
	for _, p := range dst.Packages() {
		if !src.HasPkgID(p.PkgID) {
			return fmt.Errorf("the destination holds %s, which the source repository does not have", p.NEVRA())
		}
	}
	return nil
}

// XMLBases returns the distinct <location xml:base> values the packages use, in
// sorted order. A non-empty result means the metadata pins packages to another
// host, so a client reading the copy would still download them from there.
func XMLBases(pkgs []*repodata.Package) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range pkgs {
		if p.XMLBase == "" || seen[p.XMLBase] {
			continue
		}
		seen[p.XMLBase] = true
		out = append(out, p.XMLBase)
	}
	sort.Strings(out)
	return out
}
