package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/repodata"
)

// ReasonCopied is the UploadAction reason recorded for an RPM the copy command
// already transferred directly, outside the staged-upload path. Callers that
// report a plan can recognize it to avoid listing those packages twice.
const ReasonCopied = "copied from the source repository"

// UploadAction describes what will happen (or happened) to one RPM.
type UploadAction struct {
	Location string // destination href
	Local    string // source path
	Size     int64
	Upload   bool   // true if the file is/was transferred
	Reason   string // human-readable explanation
}

// CopyAction describes a server-side relocation of an RPM: an identical copy
// already present in the repository at From is duplicated to To (instead of
// re-uploading it), and From is removed during garbage collection.
type CopyAction struct {
	From string
	To   string
	Size int64
}

// Plan summarizes a commit: the metadata that will be written and the set of
// RPM transfers, relocations, skips and deletions.
type Plan struct {
	Packages      int
	Uploads       []UploadAction // RPMs that need transferring
	Skipped       []UploadAction // RPMs already present (validated remotely)
	Copies        []CopyAction   // RPMs relocated server-side (no re-upload)
	DeletedRPMs   []string       // RPM hrefs no longer referenced, to delete
	MetadataFiles []string       // new repodata files to write
	ObsoleteMeta  []string       // superseded repodata files to delete
	BytesToUpload int64          // RPM bytes (skips + relocations excluded)
	Signed        bool

	// UnreferencedRPMs and StaleMetadata are populated only when the
	// corresponding Options are set (rebuild --remove-*). They list files found
	// in the backend that the published state no longer references and that will
	// be deleted during garbage collection.
	UnreferencedRPMs []string
	StaleMetadata    []string
}

// metadataSet holds the generated, compressed metadata documents.
type metadataSet struct {
	repomd    []byte
	files     map[string][]byte // href -> compressed bytes
	repomdSig []byte
}

// Commit publishes the current index. It uploads only RPMs that are missing or
// differ, writes regenerated metadata under checksum-named files, swaps
// repomd.xml last (so clients never see a torn repository), then deletes
// superseded metadata (and, if configured, removed RPM blobs). When DryRun is
// set it returns the Plan without transferring anything.
func (r *Repo) Commit(ctx context.Context) (*Plan, error) {
	pkgs := r.idx.Packages()

	plan, err := r.buildPlan(ctx, pkgs)
	if err != nil {
		return nil, err
	}

	meta, err := r.generateMetadata(pkgs)
	if err != nil {
		return nil, err
	}
	for href := range meta.files {
		plan.MetadataFiles = append(plan.MetadataFiles, href)
	}
	sort.Strings(plan.MetadataFiles)
	plan.ObsoleteMeta = r.obsoleteMetadata(meta)
	plan.Signed = meta.repomdSig != nil

	if err := r.computeCleanups(ctx, plan, meta); err != nil {
		return nil, err
	}

	if r.opt.DryRun {
		return plan, nil
	}

	// 1. Relocate identical RPMs server-side (write the new copy now; the old
	// copy is removed during GC, after the metadata swap, so the repo is never
	// torn).
	for _, c := range plan.Copies {
		if err := r.copyFile(ctx, c.From, c.To); err != nil {
			return nil, fmt.Errorf("relocate %s -> %s: %w", c.From, c.To, err)
		}
	}

	// 2. Upload RPMs that need transferring.
	for _, a := range plan.Uploads {
		if err := r.uploadFile(ctx, a.Location, a.Local); err != nil {
			return nil, fmt.Errorf("upload %s: %w", a.Location, err)
		}
	}

	// 3. Write new metadata blobs (checksum-named; no collision with current).
	for href, data := range meta.files {
		if err := r.putBytes(ctx, href, data); err != nil {
			return nil, fmt.Errorf("write metadata %s: %w", href, err)
		}
	}

	// 4. Swap repomd.xml last — this is the atomic publish point.
	if err := r.putBytes(ctx, repomdPath, meta.repomd); err != nil {
		return nil, fmt.Errorf("write %s: %w", repomdPath, err)
	}
	if meta.repomdSig != nil {
		if err := r.putBytes(ctx, repomdPath+".asc", meta.repomdSig); err != nil {
			return nil, fmt.Errorf("write repomd.xml.asc: %w", err)
		}
	}

	// 5. Garbage-collect superseded metadata and RPMs no longer referenced
	// (including the sources of any relocations performed in step 1).
	for _, href := range plan.ObsoleteMeta {
		if err := r.be.Delete(ctx, href); err != nil {
			return nil, fmt.Errorf("delete obsolete %s: %w", href, err)
		}
	}
	for _, href := range plan.DeletedRPMs {
		if err := r.be.Delete(ctx, href); err != nil {
			return nil, fmt.Errorf("delete rpm %s: %w", href, err)
		}
	}
	// 6. Directory-scan cleanups (rebuild --remove-*): delete files the backend
	// holds that the published state no longer references.
	for _, href := range plan.UnreferencedRPMs {
		if err := r.be.Delete(ctx, href); err != nil {
			return nil, fmt.Errorf("delete unreferenced rpm %s: %w", href, err)
		}
	}
	for _, href := range plan.StaleMetadata {
		if err := r.be.Delete(ctx, href); err != nil {
			return nil, fmt.Errorf("delete stale metadata %s: %w", href, err)
		}
	}
	return plan, nil
}

// computeCleanups populates plan.UnreferencedRPMs and plan.StaleMetadata by
// listing the backend and diffing against the published state. It is a no-op
// unless a RemoveUnreferencedRPMs/RemoveStaleMetadata option is set, and errors
// if the backend cannot enumerate its objects.
func (r *Repo) computeCleanups(ctx context.Context, plan *Plan, meta *metadataSet) error {
	if !r.opt.RemoveUnreferencedRPMs && !r.opt.RemoveStaleMetadata {
		return nil
	}
	lister, ok := r.be.(backend.Lister)
	if !ok {
		return fmt.Errorf("backend %s cannot list its contents; --remove-unreferenced-rpms/--remove-stale-metadata are unavailable here", r.be)
	}

	if r.opt.RemoveUnreferencedRPMs {
		referenced := make(map[string]bool)
		for _, p := range r.idx.Packages() {
			referenced[p.Location] = true
		}
		objs, err := lister.List(ctx, "")
		if err != nil {
			return fmt.Errorf("list repository: %w", err)
		}
		deleting := toSet(plan.DeletedRPMs) // orphans already scheduled
		for _, o := range objs {
			if !isRPM(o.Path) || referenced[o.Path] || deleting[o.Path] {
				continue
			}
			plan.UnreferencedRPMs = append(plan.UnreferencedRPMs, o.Path)
		}
		sort.Strings(plan.UnreferencedRPMs)
	}

	if r.opt.RemoveStaleMetadata {
		keep := map[string]bool{repomdPath: true, repomdPath + ".asc": true}
		for href := range meta.files {
			keep[href] = true
		}
		obsolete := toSet(plan.ObsoleteMeta) // deleted anyway; avoid double-listing
		objs, err := lister.List(ctx, "repodata/")
		if err != nil {
			return fmt.Errorf("list repodata: %w", err)
		}
		for _, o := range objs {
			if keep[o.Path] || obsolete[o.Path] {
				continue
			}
			plan.StaleMetadata = append(plan.StaleMetadata, o.Path)
		}
		sort.Strings(plan.StaleMetadata)
	}
	return nil
}

func toSet(items []string) map[string]bool {
	s := make(map[string]bool, len(items))
	for _, it := range items {
		s[it] = true
	}
	return s
}

// buildPlan decides, for each staged RPM, whether it must be uploaded,
// relocated from an existing copy, or skipped, and which now-unreferenced files
// must be garbage-collected. It uses remote validation (RemoteHasher / size) to
// avoid re-uploading files that are already present and never downloads an RPM.
func (r *Repo) buildPlan(ctx context.Context, pkgs []*repodata.Package) (*Plan, error) {
	plan := &Plan{Packages: len(pkgs)}

	// Locations the freshly-generated metadata references.
	referenced := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		referenced[p.Location] = true
	}

	// orphans: files referenced by the metadata as loaded but no longer
	// referenced now. They are garbage-collected after the metadata swap; an
	// orphan whose content (pkgid) matches a staged upload can serve as the
	// source of a server-side relocation instead of re-uploading.
	orphans := map[string]string{} // href -> original pkgid
	orphanByPkgID := map[string]string{}
	for href, id := range r.original {
		if referenced[href] {
			continue
		}
		orphans[href] = id
		if _, ok := orphanByPkgID[id]; !ok {
			orphanByPkgID[id] = href
		}
	}

	_, canCopy := r.be.(backend.Copier)

	// Deterministic order for staged uploads.
	hrefs := make([]string, 0, len(r.pending))
	for href := range r.pending {
		hrefs = append(hrefs, href)
	}
	sort.Strings(hrefs)

	hasher, _ := r.be.(backend.RemoteHasher)
	for _, href := range hrefs {
		local := r.pending[href]
		pkg := r.idx.ByLocation(href)
		action := UploadAction{Location: href, Local: local}
		if pkg != nil {
			action.Size = pkg.SizePackage
		}

		fi, err := r.be.Stat(ctx, href)
		missing := false
		switch {
		case errors.Is(err, backend.ErrNotExist):
			missing = true
			action.Upload = true
			action.Reason = "not present"
		case err != nil:
			return nil, fmt.Errorf("stat %s: %w", href, err)
		default:
			action.Upload, action.Reason, err = r.resolveExisting(ctx, href, pkg, fi, hasher)
			if err != nil {
				return nil, err
			}
		}

		// If the file is missing but an identical copy already exists in the
		// repository at a now-unreferenced location, relocate it server-side
		// instead of re-uploading. --force always re-uploads from local (it
		// deliberately does not trust the remote copy).
		if missing && !r.opt.Force && canCopy && pkg != nil {
			if from, ok := orphanByPkgID[pkg.PkgID]; ok {
				plan.Copies = append(plan.Copies, CopyAction{From: from, To: href, Size: action.Size})
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

	// Relocated existing packages: referenced by the new metadata, not staged as
	// a local upload, and at a location the loaded metadata did not have (their
	// location changed, e.g. rebuild --location-prefix). Their content already
	// lives in the repository at a now-orphaned location with the same pkgid, so
	// move it server-side instead of re-uploading. This pass is a no-op for
	// add/remove/create, where every referenced location is either pending or
	// unchanged (still in r.original).
	for _, p := range pkgs {
		if _, isPending := r.pending[p.Location]; isPending {
			continue
		}
		if _, wasOriginal := r.original[p.Location]; wasOriginal {
			continue
		}
		// Already transferred out of band by the copy command (or, under
		// --dry-run, accounted for as if it had been).
		if size, wasCopied := r.copied[p.Location]; wasCopied {
			plan.Skipped = append(plan.Skipped, UploadAction{Location: p.Location, Size: size, Reason: ReasonCopied})
			continue
		}
		// A new location for existing content. If it is already present (e.g. a
		// resumed rebuild), trust it; otherwise relocate from the orphan copy.
		if _, err := r.be.Stat(ctx, p.Location); err == nil {
			plan.Skipped = append(plan.Skipped, UploadAction{Location: p.Location, Size: p.SizePackage, Reason: "present at new location"})
			continue
		} else if !errors.Is(err, backend.ErrNotExist) {
			return nil, fmt.Errorf("stat %s: %w", p.Location, err)
		}
		if canCopy {
			if from, ok := orphanByPkgID[p.PkgID]; ok {
				plan.Copies = append(plan.Copies, CopyAction{From: from, To: p.Location, Size: p.SizePackage})
				continue
			}
		}
		return nil, fmt.Errorf("cannot relocate %s to %s: no local copy staged and backend cannot move it server-side", p.NEVRA(), p.Location)
	}

	// Everything still unreferenced is garbage. A relocation source is included
	// here too: its copy is written before the metadata swap, and the source is
	// removed afterwards.
	for href := range orphans {
		plan.DeletedRPMs = append(plan.DeletedRPMs, href)
	}
	sort.Strings(plan.DeletedRPMs)
	return plan, nil
}

// resolveExisting decides whether an already-present file must be re-uploaded.
func (r *Repo) resolveExisting(ctx context.Context, href string, pkg *repodata.Package, fi *backend.FileInfo, hasher backend.RemoteHasher) (upload bool, reason string, err error) {
	// --force means "upload regardless": overwrite the remote file even if it
	// looks identical. This is the only correct behavior when we cannot prove
	// the remote file matches, or when the local file will differ once written
	// (e.g. it was just signed).
	if r.opt.Force {
		return true, "overwriting (--force)", nil
	}
	if pkg == nil {
		// Shouldn't happen: a pending href always maps to an indexed package.
		return true, "no package metadata", nil
	}
	if hasher != nil {
		sum, ok, herr := hasher.Hash(ctx, href, r.opt.checksumType())
		if herr != nil && !errors.Is(herr, backend.ErrNotExist) {
			return false, "", fmt.Errorf("remote hash %s: %w", href, herr)
		}
		if ok {
			if sum == pkg.PkgID {
				return false, "present, checksum verified remotely", nil
			}
			return false, "", fmt.Errorf("%s already exists with different content (checksum %s != %s); use --force to overwrite", href, sum, pkg.PkgID)
		}
	}
	// Fall back to a size comparison when no checksum is available.
	if fi.Size == pkg.SizePackage {
		return false, "present, size matches (checksum not verified)", nil
	}
	return false, "", fmt.Errorf("%s already exists with different size (%d != %d); use --force to overwrite", href, fi.Size, pkg.SizePackage)
}

// generateMetadata renders and compresses primary/filelists/other and builds
// repomd.xml (optionally signed).
func (r *Repo) generateMetadata(pkgs []*repodata.Package) (*metadataSet, error) {
	now := r.nowUnix()
	comp := r.opt.Compression
	if comp == "" {
		comp = GZIP
	}
	cksum := r.opt.checksumType()

	docs := []struct {
		typ  string
		open []byte
	}{
		{"primary", repodata.WritePrimary(pkgs)},
		{"filelists", repodata.WriteFilelists(pkgs)},
		{"other", repodata.WriteOther(pkgs)},
	}

	ms := &metadataSet{files: map[string][]byte{}}
	repomd := &repodata.Repomd{Revision: fmt.Sprintf("%d", now)}
	for _, d := range docs {
		compressed, err := comp.compress(d.open)
		if err != nil {
			return nil, err
		}
		closedSum := sha256Hex(compressed)
		href := path.Join("repodata", fmt.Sprintf("%s-%s.xml%s", closedSum, d.typ, comp.ext()))
		ms.files[href] = compressed
		repomd.Data = append(repomd.Data, repodata.DataEntry{
			Type:         d.typ,
			ChecksumType: cksum,
			Checksum:     closedSum,
			OpenChecksum: sha256Hex(d.open),
			Location:     href,
			Timestamp:    now,
			Size:         int64(len(compressed)),
			OpenSize:     int64(len(d.open)),
		})
	}
	ms.repomd = repodata.WriteRepomd(repomd)

	if r.opt.Signer != nil {
		sig, err := r.opt.Signer.SignDetached(ms.repomd)
		if err != nil {
			return nil, fmt.Errorf("sign repomd.xml: %w", err)
		}
		ms.repomdSig = sig
	}
	return ms, nil
}

// obsoleteMetadata returns previously-published repodata files that the new
// metadata replaces (so they can be deleted). The current repomd.xml is left in
// place because it is overwritten, not replaced by a checksum-named file.
func (r *Repo) obsoleteMetadata(meta *metadataSet) []string {
	if r.old == nil {
		return nil
	}
	var out []string
	for _, d := range r.old.Data {
		if _, stillUsed := meta.files[d.Location]; stillUsed {
			continue
		}
		if d.Location == repomdPath {
			continue
		}
		out = append(out, d.Location)
	}
	sort.Strings(out)
	return out
}

func (r *Repo) uploadFile(ctx context.Context, href, local string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	return r.be.Put(ctx, href, f, fi.Size())
}

func (r *Repo) putBytes(ctx context.Context, href string, data []byte) error {
	return r.be.Put(ctx, href, bytesReader(data), int64(len(data)))
}

// copyFile relocates an existing RPM within the backend without transferring
// its bytes. It is only called for actions the plan produced, which requires
// the backend to be a Copier.
func (r *Repo) copyFile(ctx context.Context, from, to string) error {
	c, ok := r.be.(backend.Copier)
	if !ok {
		return fmt.Errorf("backend does not support server-side copy")
	}
	return c.Copy(ctx, from, to)
}
