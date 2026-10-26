package repo

import (
	"context"
	"fmt"
	"path"
	"sort"

	"github.com/danudey/createrepo-go/pkg/publish"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/rpm/repodata"
)

// ReasonCopied is the UploadAction reason recorded for an RPM the copy command
// already transferred directly, outside the staged-upload path. Callers that
// report a plan can recognize it to avoid listing those packages twice.
const ReasonCopied = publish.ReasonCopied

// UploadAction describes what will happen (or happened) to one RPM.
type UploadAction = publish.UploadAction

// CopyAction describes a server-side relocation of an RPM: an identical copy
// already present in the repository at From is duplicated to To (instead of
// re-uploading it), and From is removed during garbage collection.
type CopyAction = publish.CopyAction

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

	steps := publish.Steps{
		Copies:  plan.Copies,
		Uploads: plan.Uploads,
		Swap:    []publish.File{{Path: repomdPath, Data: meta.repomd}},
		Guard:   []publish.Unchanged{{Path: repomdPath, Exists: r.raw != nil, Data: r.raw}},
		GC:      [][]string{plan.ObsoleteMeta, plan.DeletedRPMs, plan.UnreferencedRPMs, plan.StaleMetadata},
	}
	// The data files are checksum-named, so they never collide with what the
	// current repomd.xml names.
	for _, href := range plan.MetadataFiles {
		steps.Metadata = append(steps.Metadata, publish.File{Path: href, Data: meta.files[href]})
	}
	if meta.repomdSig != nil {
		steps.Swap = append(steps.Swap, publish.File{Path: repomdPath + ".asc", Data: meta.repomdSig})
	} else if r.old != nil {
		// The repository was signed before and is not now. A signature left
		// behind no longer matches the new repomd.xml, and a client with
		// repo_gpgcheck=1 would reject the repository rather than treat it as
		// unsigned.
		steps.Retract = []string{repomdPath + ".asc"}
	}
	if err := publish.Execute(ctx, r.be, steps, r.opt.Tracker); err != nil {
		return nil, err
	}
	// A later Commit on this Repo builds on what this one published.
	r.raw = meta.repomd
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
		keep := func(p string) bool { return !isRPM(p) || referenced[p] }
		var err error
		// Orphans already scheduled for deletion are not listed twice.
		plan.UnreferencedRPMs, err = publish.Unreferenced(ctx, lister, "", keep, plan.DeletedRPMs)
		if err != nil {
			return fmt.Errorf("list repository: %w", err)
		}
	}

	if r.opt.RemoveStaleMetadata {
		keep := map[string]bool{repomdPath: true, repomdPath + ".asc": true}
		for href := range meta.files {
			keep[href] = true
		}
		var err error
		// Superseded metadata is deleted anyway, so it is not listed twice.
		plan.StaleMetadata, err = publish.Unreferenced(ctx, lister, "repodata/", func(p string) bool { return keep[p] }, plan.ObsoleteMeta)
		if err != nil {
			return fmt.Errorf("list repodata: %w", err)
		}
	}
	return nil
}

// buildPlan decides, for each staged RPM, whether it must be uploaded,
// relocated from an existing copy, or skipped, and which now-unreferenced files
// must be garbage-collected (see publish.PlanFiles).
func (r *Repo) buildPlan(ctx context.Context, pkgs []*repodata.Package) (*Plan, error) {
	referenced := make(map[string]publish.Expect, len(pkgs))
	for _, p := range pkgs {
		referenced[p.Location] = publish.Expect{
			Size: p.SizePackage, SHA256: sha256Only(p.PkgID, p.ChecksumType), ID: p.PkgID,
		}
	}
	files, err := publish.PlanFiles(ctx, r.be, publish.State{
		Referenced: referenced,
		Original:   r.original,
		Pending:    r.pending,
		Copied:     r.copied,
		Force:      r.opt.Force,
		Describe: func(href string) string {
			if p := r.idx.ByLocation(href); p != nil {
				return p.NEVRA() + " at " + href
			}
			return href
		},
	})
	if err != nil {
		return nil, err
	}
	return &Plan{
		Packages:      len(pkgs),
		Uploads:       files.Uploads,
		Skipped:       files.Skipped,
		Copies:        files.Copies,
		DeletedRPMs:   files.Deleted,
		BytesToUpload: files.BytesToUpload,
	}, nil
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
