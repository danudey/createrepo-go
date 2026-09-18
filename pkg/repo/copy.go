package repo

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
	"github.com/danudey/createrepo-go/pkg/repodata"
	"github.com/danudey/createrepo-go/pkg/rpmmeta"
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
// `copy --overwrite`, which replaces a destination repository wholesale.
func (r *Repo) ClearIndex() {
	for _, p := range r.idx.Packages() {
		r.idx.RemoveByPkgID(p.PkgID)
	}
}

// ObjectKind labels a repository file by the role it plays.
type ObjectKind string

// The kinds of file a repository is made of. Copying treats each kind
// differently: repomd and its signature are written last, packages may be
// skipped when already present, and anything "other" is carried across as-is.
const (
	ObjectRepomd    ObjectKind = "repomd"    // repodata/repomd.xml
	ObjectRepomdSig ObjectKind = "signature" // repodata/repomd.xml.asc
	ObjectMetadata  ObjectKind = "metadata"  // an index file repomd.xml references
	ObjectPackage   ObjectKind = "package"   // an RPM the metadata references
	ObjectConfig    ObjectKind = "config"    // createrepo-go.json
	ObjectOther     ObjectKind = "other"     // anything else found in the backend
)

// SourceObject is one file that makes up a repository, together with whatever
// integrity data the metadata records for it. Checksum is empty for files the
// metadata says nothing about (repomd.xml itself, and any stray file).
type SourceObject struct {
	Path         string
	Kind         ObjectKind
	Size         int64 // -1 when unknown
	Checksum     string
	ChecksumType string
}

// verifiable reports whether the object carries a checksum this tool can check.
func (o SourceObject) verifiable() bool {
	return o.Checksum != "" && strings.EqualFold(o.ChecksumType, backend.AlgoSHA256)
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
			Checksum: d.Checksum, ChecksumType: d.ChecksumType,
		})
	}
	for _, p := range r.idx.Packages() {
		add(SourceObject{
			Path: p.Location, Kind: ObjectPackage, Size: p.SizePackage,
			Checksum: p.PkgID, ChecksumType: p.ChecksumType,
		})
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
		objs = append(objs, o)
	}
	sortObjects(objs)
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

// sortObjects puts the objects into publish order: everything a client might
// need first, then repomd.xml (the atomic publish point), then its signature.
// Within a group the order is alphabetical, for reproducible output.
func sortObjects(objs []SourceObject) {
	rank := func(o SourceObject) int {
		switch o.Kind {
		case ObjectRepomd:
			return 1
		case ObjectRepomdSig:
			return 2
		default:
			return 0
		}
	}
	sort.Slice(objs, func(i, j int) bool {
		if ri, rj := rank(objs[i]), rank(objs[j]); ri != rj {
			return ri < rj
		}
		return objs[i].Path < objs[j].Path
	})
}

// CopyOptions controls a verbatim repository copy.
type CopyOptions struct {
	// DryRun computes the transfer without writing anything.
	DryRun bool
	// Force overwrites a destination object whose content differs from the
	// source's instead of failing.
	Force bool
	// Prune deletes destination objects the source does not have, making the
	// destination an exact replica. It requires a destination that can list its
	// contents.
	Prune bool
	// Inspect is called with the local temporary copy of each transferred
	// object once its checksum has been validated and before it is written to
	// the destination. Returning an error aborts the copy. It is where package
	// signature verification hooks in.
	Inspect func(obj SourceObject, localPath string) error
	// Progress reports each object as it is handled. action is one of "copy",
	// "skip" or "delete".
	Progress func(action string, obj SourceObject)
}

// CopyStats summarizes what a copy transferred.
type CopyStats struct {
	Copied  int
	Skipped int
	Bytes   int64
	Deleted []string
}

// CopyExact replicates objs from src to dst byte for byte, verifying every
// object whose checksum the metadata records. An object already present at the
// destination with matching content is skipped, which is what makes an
// interrupted copy resumable. Objects are written in the order given, so
// repomd.xml lands last and the destination is never a torn repository.
func CopyExact(ctx context.Context, src, dst backend.Backend, objs []SourceObject, opt CopyOptions) (*CopyStats, error) {
	stats := &CopyStats{}
	progress := opt.Progress
	if progress == nil {
		progress = func(string, SourceObject) {}
	}

	for _, obj := range objs {
		present, err := destinationMatches(ctx, dst, obj)
		if err != nil {
			return nil, err
		}
		if present && !opt.Force {
			stats.Skipped++
			progress("skip", obj)
			continue
		}
		// A dry run reports the transfer without performing it, so it stays
		// cheap even for a repository of any size.
		if opt.DryRun {
			stats.Copied++
			if obj.Size > 0 {
				stats.Bytes += obj.Size
			}
			progress("copy", obj)
			continue
		}

		local, size, err := fetchToTemp(ctx, src, obj)
		if err != nil {
			return nil, err
		}
		if opt.Inspect != nil {
			if err := opt.Inspect(obj, local); err != nil {
				os.Remove(local)
				return nil, err
			}
		}
		stats.Copied++
		stats.Bytes += size
		progress("copy", obj)
		err = putFile(ctx, dst, obj.Path, local)
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
func pruneExtraneous(ctx context.Context, dst backend.Backend, objs []SourceObject, opt CopyOptions) ([]string, error) {
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
		if keep[o.Path] {
			continue
		}
		deleted = append(deleted, o.Path)
	}
	sort.Strings(deleted)
	if opt.Progress != nil {
		for _, p := range deleted {
			opt.Progress("delete", SourceObject{Path: p, Kind: classifyObject(p)})
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

// destinationMatches reports whether dst already holds an object identical to
// the source's. A checksum decides it where one is available (recorded in the
// metadata and computable remotely); otherwise the size has to stand in.
func destinationMatches(ctx context.Context, dst backend.Backend, obj SourceObject) (bool, error) {
	fi, err := dst.Stat(ctx, obj.Path)
	if errors.Is(err, backend.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", obj.Path, err)
	}
	if obj.verifiable() {
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
	switch obj.Kind {
	case ObjectRepomd, ObjectRepomdSig, ObjectConfig:
		// No checksum is recorded for these, and they are exactly the files
		// whose content changes without changing length — repomd.xml differs
		// from a previous generation only in its revision. They are small, so
		// they are always rewritten rather than compared by size.
		return false, nil
	case ObjectMetadata, ObjectPackage, ObjectOther:
		// Content-addressed or immutable once written, so an equal length
		// means an equal file.
	}
	return obj.Size >= 0 && fi.Size == obj.Size, nil
}

// fetchToTemp streams an object from be into a temporary file, verifying its
// size and (when the metadata records one) its checksum. The caller owns the
// returned file and must remove it.
func fetchToTemp(ctx context.Context, be backend.Backend, obj SourceObject) (string, int64, error) {
	rc, err := be.Get(ctx, obj.Path)
	if err != nil {
		return "", 0, fmt.Errorf("read %s: %w", obj.Path, err)
	}
	defer rc.Close()

	f, err := os.CreateTemp("", "cr-copy-*-"+path.Base(obj.Path))
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
	size, err := io.Copy(io.MultiWriter(f, h), rc)
	if err != nil {
		return fail(fmt.Errorf("read %s: %w", obj.Path, err))
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", 0, err
	}
	if obj.Size >= 0 && size != obj.Size {
		os.Remove(name)
		return "", 0, fmt.Errorf("%s: got %d bytes, metadata says %d", obj.Path, size, obj.Size)
	}
	if obj.verifiable() {
		if sum := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(sum, obj.Checksum) {
			os.Remove(name)
			return "", 0, fmt.Errorf("%s: checksum %s does not match the %s the metadata records",
				obj.Path, sum, obj.Checksum)
		}
	}
	return name, size, nil
}

// putFile uploads a local file to a backend.
func putFile(ctx context.Context, be backend.Backend, href, local string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	return be.Put(ctx, href, f, fi.Size())
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
	progress := opt.Progress
	if progress == nil {
		progress = func(string, *repodata.Package, string) {}
	}

	for _, p := range pkgs {
		loc := r.copyLocation(p, opt)
		obj := SourceObject{
			Path: p.Location, Kind: ObjectPackage, Size: p.SizePackage,
			Checksum: p.PkgID, ChecksumType: p.ChecksumType,
		}

		// Signing rewrites the file, so a copy already at the destination is
		// never reusable and the metadata always has to be re-derived.
		rewrites := opt.Sign != nil
		if !rewrites && !opt.RebuildMetadata {
			present, err := destinationMatches(ctx, r.be, SourceObject{
				Path: loc, Kind: ObjectPackage, Size: p.SizePackage,
				Checksum: p.PkgID, ChecksumType: p.ChecksumType,
			})
			if err != nil {
				return nil, err
			}
			if present {
				r.addCopied(p, loc, opt)
				stats.Skipped++
				progress("skip", p, loc)
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
			progress("copy", p, loc)
			continue
		}

		local, size, err := fetchToTemp(ctx, src, obj)
		if err != nil {
			return nil, err
		}
		if opt.Inspect != nil {
			if err := opt.Inspect(p, local); err != nil {
				os.Remove(local)
				return nil, err
			}
		}

		// Every temporary file this iteration created is released before the
		// next package is fetched, so disk use stays bounded by one package.
		upload, release := local, func() { os.Remove(local) }
		if opt.Sign != nil {
			signed, cleanup, err := opt.Sign(local)
			os.Remove(local)
			if err != nil {
				return nil, fmt.Errorf("sign %s: %w", p.NEVRA(), err)
			}
			upload, release = signed, cleanup
		}

		var indexed *repodata.Package
		if rewrites || opt.RebuildMetadata {
			indexed, err = r.addFromFile(upload, loc, opt)
			if err != nil {
				release()
				return nil, fmt.Errorf("index %s: %w", p.NEVRA(), err)
			}
		} else {
			indexed = r.addCopied(p, loc, opt)
		}

		stats.Copied++
		stats.Bytes += size
		progress("copy", p, loc)

		if err := putFile(ctx, r.be, loc, upload); err != nil {
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
	pkg, err := rpmmeta.FromFile(localPath, rpmmeta.Options{
		ChecksumType:   r.opt.checksumType(),
		ChangelogLimit: r.opt.ChangelogLimit,
	})
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
