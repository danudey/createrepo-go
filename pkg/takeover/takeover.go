// Package takeover analyses a repository that was not created with this tool
// and reports, without changing anything, exactly what publishing it with a
// given set of settings would do to it: which metadata would be regenerated,
// which would be dropped, which files would be written, moved or deleted, and
// which of those changes its clients would notice.
//
// The analysis is a real dry run rather than a description of one: the
// repository is loaded, the requested reconciliation (re-reading the packages,
// relocating them, pruning superseded versions) is applied to the in-memory
// index, and the commit plan is computed from it. Nothing is transferred and
// nothing is written, so the answer can be obtained against a live repository
// before deciding whether to take it over.
package takeover

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/repo"
	"github.com/danudey/createrepo-go/pkg/repoconfig"
	"github.com/danudey/createrepo-go/pkg/repodata"
	"github.com/danudey/createrepo-go/pkg/sign"
)

// maxItems is how many subjects a finding lists before it summarizes the rest.
const maxItems = 5

// Options are the settings a prospective takeover would publish with, plus the
// analysis's own knobs. They mirror the command-line flags: the point of the
// analysis is to say what these particular settings would do.
type Options struct {
	Target         string // compatibility profile in effect, if any
	Compression    string // metadata compression that would be written
	ChecksumType   string // metadata/package checksum that would be used
	LocationPrefix string // subdirectory RPMs would be published under
	// Relocate reports that LocationPrefix was given explicitly, which is what
	// makes a republish move the existing packages. Without it they keep the
	// locations the published metadata gives them.
	Relocate bool
	// FromPackages re-reads every RPM and regenerates its metadata from the
	// file, which is the only way the analysis can tell whether the published
	// metadata describes what is actually stored.
	FromPackages   bool
	ChangelogLimit int
	PruneOlder     bool

	SignMetadata    bool
	SignPackages    bool
	SignatureFormat string
	// GPGKeyID is the key that would sign, as supplied (a fingerprint, key id
	// or uid).
	GPGKeyID string

	// SamplePackages is how many packages to download and inspect to find out
	// who signed them. Zero skips the inspection.
	SamplePackages int

	// Name and BaseURL are recorded in the proposed configuration.
	Name, BaseURL string

	// ExistingConfig is the repository's createrepo-go.json, when it has one.
	ExistingConfig *repoconfig.Config

	// Progress, if set, is called with a line describing each step that costs
	// a transfer, so a slow analysis does not look like a hang.
	Progress func(string)
}

// Analyze inspects the repository r was opened on and reports what taking it
// over with opt would change. It mutates r's in-memory index (that is how the
// plan is computed) but never writes to the backend, so the repository is left
// exactly as it was found.
func Analyze(ctx context.Context, r *repo.Repo, opt Options) (*Report, error) {
	a := &analysis{
		r:   r,
		opt: opt,
		rep: &Report{
			Location: r.Backend().String(),
			Managed:  opt.ExistingConfig != nil,
			Proposed: Proposed{
				Target: opt.Target, Compression: opt.Compression, ChecksumType: opt.ChecksumType,
				LocationPrefix: opt.LocationPrefix, Relocate: opt.Relocate,
				FromPackages: opt.FromPackages, ChangelogLimit: opt.ChangelogLimit,
				PruneOlder: opt.PruneOlder, SignMetadata: opt.SignMetadata,
				SignPackages: opt.SignPackages, SignatureFormat: opt.SignatureFormat,
				GPGKeyID: opt.GPGKeyID,
			},
		},
	}
	if err := a.run(ctx); err != nil {
		return nil, err
	}
	return a.rep, nil
}

// analysis carries the state shared by the analysis steps.
type analysis struct {
	r   *repo.Repo
	opt Options
	rep *Report

	// The repository's metadata as published, parsed before anything is
	// reconciled, so the regenerated metadata can be diffed against it.
	published []*repodata.Package
	filelists map[string][]repodata.File
	changelog map[string][]repodata.Changelog

	// The backend's own listing, when it can produce one.
	sizes  map[string]int64
	listed bool
}

func (a *analysis) run(ctx context.Context) error {
	if a.r.Repomd() == nil {
		return fmt.Errorf("no repository metadata at %s: there is nothing to take over", a.r.Backend())
	}
	if err := a.loadPublished(ctx); err != nil {
		return err
	}
	if err := a.listBackend(ctx); err != nil {
		return err
	}
	a.inventory()
	a.metadataFates()
	a.packageChecksums()
	a.packageLocations()
	a.files()
	if err := a.signatures(ctx); err != nil {
		return err
	}
	if err := a.reconcile(ctx); err != nil {
		return err
	}
	if err := a.plan(ctx); err != nil {
		return err
	}
	a.diff()
	a.dependencies()
	a.proposeConfig()
	a.rep.sortFindings()
	return nil
}

// loadPublished parses the repository's metadata documents as they stand, so
// the analysis keeps a view of what is published even after the in-memory index
// has been reconciled.
func (a *analysis) loadPublished(ctx context.Context) error {
	primary, err := a.r.MetadataDocument(ctx, "primary")
	if err != nil {
		return err
	}
	if a.published, err = repodata.ParsePrimary(primary); err != nil {
		return err
	}
	if fl, err := a.r.MetadataDocument(ctx, "filelists"); err != nil {
		return err
	} else if fl != nil {
		if a.filelists, err = repodata.ParseFilelists(fl); err != nil {
			return err
		}
	}
	if ot, err := a.r.MetadataDocument(ctx, "other"); err != nil {
		return err
	} else if ot != nil {
		if a.changelog, err = repodata.ParseOther(ot); err != nil {
			return err
		}
	}
	return nil
}

// listBackend records the size of every object the backend holds. A backend
// that cannot enumerate itself (plain HTTP) leaves sizes nil, and the analysis
// then reports only what the metadata accounts for.
func (a *analysis) listBackend(ctx context.Context) error {
	lister, ok := a.r.Backend().(backend.Lister)
	if !ok {
		return nil
	}
	objs, err := lister.List(ctx, "")
	if err != nil {
		return fmt.Errorf("list %s: %w", a.r.Backend(), err)
	}
	a.sizes = make(map[string]int64, len(objs))
	for _, o := range objs {
		a.sizes[o.Path] = o.Size
	}
	a.listed = true
	return nil
}

// inventory fills in the published state: package counts and sizes, the
// metadata documents repomd.xml names, and the compressions and checksum types
// in use.
func (a *analysis) inventory() {
	pub := &a.rep.Published
	repomd := a.r.Repomd()
	pub.Revision = repomd.Revision
	pub.Listed = a.listed

	var comps, cksums, dirs, bases []string
	for _, p := range a.published {
		pub.Packages++
		pub.PackageBytes += p.SizePackage
		cksums = append(cksums, strings.ToLower(p.ChecksumType))
		dirs = append(dirs, dirOf(p.Location))
		bases = append(bases, p.XMLBase)
	}
	// A package at the repository root has no directory; record that explicitly
	// rather than losing it to uniqueSorted's empty-string filter.
	for _, p := range a.published {
		if dirOf(p.Location) == "" {
			dirs = append(dirs, "(repository root)")
			break
		}
	}

	for _, d := range repomd.Data {
		comp := compressionOf(d.Location)
		comps = append(comps, comp)
		fate := "deleted"
		if generated[d.Type] {
			fate = "regenerated"
		}
		pub.Metadata = append(pub.Metadata, MetadataEntry{
			Type: d.Type, Location: d.Location, Compression: comp,
			ChecksumType: strings.ToLower(d.ChecksumType), Size: d.Size, Fate: fate,
		})
	}
	pub.Compressions = uniqueSorted(comps)
	pub.ChecksumTypes = uniqueSorted(cksums)
	pub.PackageDirs = uniqueSorted(dirs)
	pub.XMLBases = uniqueSorted(bases)

	if a.listed {
		for _, size := range a.sizes {
			pub.TotalBytes += size
			pub.TotalFiles++
		}
	} else {
		pub.TotalBytes = pub.PackageBytes
		pub.TotalFiles = pub.Packages
		for _, d := range repomd.Data {
			pub.TotalBytes += d.Size
			pub.TotalFiles++
		}
	}

	if backend.IsReadOnly(a.r.Backend()) {
		a.rep.add(Finding{
			Severity: Blocking, Code: "read-only-backend",
			Summary: fmt.Sprintf("%s can be read but never written", a.r.Backend()),
			Detail: "the analysis is accurate, but this repository cannot be republished where it stands: " +
				"an HTTP(S) location is read-only.",
			Fix: "copy it to a writable location first (createrepo-go copy <this url> <target>), and take that over",
		})
	}
	if !a.listed {
		a.rep.add(Finding{
			Severity: Advisory, Code: "backend-cannot-list",
			Summary: fmt.Sprintf("%s cannot enumerate its contents", a.r.Backend()),
			Detail: "only the files the metadata references are visible, so any package, stale index file or " +
				"unrelated file the repository also holds is invisible to this analysis and is not accounted for below.",
		})
	}
	if a.rep.Managed {
		a.rep.add(Finding{
			Severity: Info, Code: "already-managed",
			Summary: "the repository already carries a " + repoconfig.Path,
			Detail:  "it has been published by this tool before, so its recorded settings are being used as defaults here.",
		})
	}
}

// metadataFates reports the documents a republish would drop. The tool
// generates primary, filelists and other and nothing else, and a commit deletes
// the files the superseded repomd.xml referenced — so every other document the
// repository publishes goes away, and its consumers with it.
func (a *analysis) metadataFates() {
	byImpact := map[string][]string{}
	worst := map[string]metadataImpact{}
	for _, d := range a.r.Repomd().Data {
		if generated[d.Type] {
			continue
		}
		im := impactOf(d.Type)
		byImpact[im.what] = append(byImpact[im.what], fmt.Sprintf("%s (%s)", d.Type, d.Location))
		worst[im.what] = im
	}
	kinds := make([]string, 0, len(byImpact))
	for what := range byImpact {
		kinds = append(kinds, what)
	}
	sort.Strings(kinds)

	for _, what := range kinds {
		im := worst[what]
		a.rep.add(Finding{
			Severity: im.severity, Code: "metadata-dropped",
			Summary: fmt.Sprintf("%s would be dropped: this tool generates only primary, filelists and other", what),
			Detail:  im.consequence + ". The superseded files are deleted when the new repomd.xml is published.",
			Fix:     im.fix,
			Items:   truncate(byImpact[what], maxItems),
		})
	}
}

// packageChecksums reports a repository whose packages are indexed with
// anything other than the checksum this tool computes. It is the quietest way a
// takeover goes wrong: the metadata stays readable by dnf, but every later
// operation of this tool compares a sha256 it computed against a pkgid that is
// not one.
func (a *analysis) packageChecksums() {
	want := strings.ToLower(a.opt.ChecksumType)
	if want == "" {
		want = backend.AlgoSHA256
	}
	var others []string
	for _, p := range a.published {
		if got := strings.ToLower(p.ChecksumType); got != want {
			others = append(others, fmt.Sprintf("%s (%s)", p.NEVRA(), got))
		}
	}
	if len(others) == 0 {
		return
	}
	if a.opt.FromPackages {
		a.rep.add(Finding{
			Severity: Advisory, Code: "package-checksum-type",
			Summary: fmt.Sprintf("%s indexed with a checksum other than %s; re-reading the RPMs recomputes them",
				plural(len(others), "package is", "packages are"), want),
			Detail: fmt.Sprintf("--from-packages re-reads each RPM, so every one of these gets a new %s pkgid. "+
				"That is the correct outcome, but it does mean the pkgid of every affected package changes.", want),
			Items: truncate(others, maxItems),
		})
		return
	}
	a.rep.add(Finding{
		Severity: Blocking, Code: "package-checksum-type",
		Summary: fmt.Sprintf("%s indexed with a checksum other than %s",
			plural(len(others), "package is", "packages are"), want),
		Detail: fmt.Sprintf("the republished metadata would keep those pkgid values, but this tool computes %s "+
			"for every package it checks: verify would report a mismatch on each one, and adding a package that is "+
			"already present would fail with \"already exists with different content\".", want),
		Fix:   "--from-packages, which re-reads every RPM and recomputes its pkgid",
		Items: truncate(others, maxItems),
	})
}

// packageLocations reports where the packages live and what the configured
// --location-prefix would do about it.
func (a *analysis) packageLocations() {
	dirs := a.rep.Published.PackageDirs
	prefix := strings.Trim(a.opt.LocationPrefix, "/")

	if a.opt.Relocate {
		return // the relocation itself is reported from the plan
	}
	if len(dirs) == 0 {
		return
	}
	atRoot := false
	mismatched := []string{}
	for _, d := range dirs {
		switch {
		case d == "(repository root)":
			atRoot = true
		case d != prefix:
			mismatched = append(mismatched, d)
		}
	}
	if atRoot && prefix != "" {
		a.rep.add(Finding{
			Severity: Advisory, Code: "location-prefix-mismatch",
			Summary: fmt.Sprintf("packages are published at the repository root, but new ones would land in %s/", prefix),
			Detail: "the existing packages are left where they are (this analysis does not move them), so the " +
				"repository would end up with packages in two places. A repository root location cannot be " +
				"recorded in " + repoconfig.Path + " either — the empty value means \"not recorded\" — so every later " +
				"add needs the flag.",
			Fix: `--location-prefix "" on every later command, or --location-prefix ` + prefix + " here to move the existing packages instead",
		})
	}
	if len(mismatched) > 0 {
		a.rep.add(Finding{
			Severity: Advisory, Code: "location-prefix-mismatch",
			Summary: fmt.Sprintf("packages are published under %s, but new ones would land in %s/",
				strings.Join(truncate(mismatched, maxItems), ", "), prefix),
			Detail: "the existing packages keep their locations, so later additions land somewhere else. " +
				"That is legal — the metadata names each package's location — but it makes the layout harder to reason about.",
			Fix: "--location-prefix " + mismatched[0] + " to keep using the existing directory",
		})
	}
	if len(a.rep.Published.XMLBases) > 0 {
		a.rep.add(Finding{
			Severity: Advisory, Code: "xml-base",
			Summary: "the metadata carries xml:base, so clients fetch the packages from another host",
			Detail: "republishing preserves it, so the repository keeps pointing its clients elsewhere no matter " +
				"where it is served from. Everything this tool reports about package files still refers to the copies " +
				"stored here.",
			Fix:   "createrepo-go copy --remove-baseurl, which rewrites the metadata to point at the repository itself",
			Items: truncate(a.rep.Published.XMLBases, maxItems),
		})
	}
}

// files reconciles the metadata against the backend's listing: packages the
// metadata references but the repository does not hold, RPMs it holds but does
// not reference, superseded index files, and anything else stored alongside.
func (a *analysis) files() {
	if !a.listed {
		return
	}
	referenced := map[string]bool{}
	for _, p := range a.published {
		referenced[p.Location] = true
	}
	metaReferenced := map[string]bool{"repodata/repomd.xml": true, "repodata/repomd.xml.asc": true}
	for _, d := range a.r.Repomd().Data {
		metaReferenced[d.Location] = true
	}

	var missing, strayRPMs, staleMeta, other []string
	var strayBytes int64
	for loc := range referenced {
		if _, ok := a.sizes[loc]; !ok {
			missing = append(missing, loc)
		}
	}
	for p, size := range a.sizes {
		switch {
		case referenced[p] || metaReferenced[p] || p == repoconfig.Path:
			continue
		case strings.EqualFold(path.Ext(p), ".rpm"):
			strayRPMs = append(strayRPMs, p)
			strayBytes += size
		case strings.HasPrefix(p, "repodata/"):
			staleMeta = append(staleMeta, p)
		default:
			other = append(other, p)
		}
	}
	sortStrings(missing)
	sortStrings(strayRPMs)
	sortStrings(staleMeta)
	sortStrings(other)

	pub := &a.rep.Published
	pub.StrayRPMs, pub.StrayRPMBytes = strayRPMs, strayBytes
	pub.StrayMetadata, pub.OtherFiles = staleMeta, other

	if len(missing) > 0 {
		a.rep.add(Finding{
			Severity: Blocking, Code: "missing-packages",
			Summary: fmt.Sprintf("%s referenced by the metadata but not stored here",
				plural(len(missing), "package is", "packages are")),
			Detail: "clients already fail to install these. A republish carries the broken references across " +
				"unchanged, and --from-packages cannot repair an entry whose file is absent.",
			Fix:   "restore the files, or drop the entries with createrepo-go remove before taking the repository over",
			Items: truncate(missing, maxItems),
		})
	}
	if len(strayRPMs) > 0 {
		a.rep.add(Finding{
			Severity: Info, Code: "unreferenced-rpms",
			Summary: fmt.Sprintf("%s stored but not referenced by the metadata (%s)",
				plural(len(strayRPMs), "RPM is", "RPMs are"), humanBytes(strayBytes)),
			Detail: "a republish leaves them exactly where they are; they are invisible to dnf either way.",
			Fix:    "rebuild --remove-unreferenced-rpms to delete them, or add them to the repository",
			Items:  truncate(strayRPMs, maxItems),
		})
	}
	if len(staleMeta) > 0 {
		a.rep.add(Finding{
			Severity: Info, Code: "stale-metadata",
			Summary: fmt.Sprintf("%s under repodata/ that the current repomd.xml does not reference",
				plural(len(staleMeta), "file is", "files are")),
			Detail: "these are left over from earlier publishes. A republish deletes only the files the current " +
				"repomd.xml names, so these stay.",
			Fix:   "rebuild --remove-stale-metadata to delete them",
			Items: truncate(staleMeta, maxItems),
		})
	}
	if len(other) > 0 {
		a.rep.add(Finding{
			Severity: Info, Code: "other-files",
			Summary: fmt.Sprintf("%s stored alongside the repository", plural(len(other), "other file is", "other files are")),
			Detail:  "a republish does not touch them.",
			Items:   truncate(other, maxItems),
		})
	}
}

// signatures establishes who signed the repository — the detached repomd.xml
// signature and, when sampling is enabled, the packages themselves — and what
// the configured signing settings would do to that.
func (a *analysis) signatures(ctx context.Context) error {
	if err := a.metadataSignature(ctx); err != nil {
		return err
	}
	return a.packageSignatures(ctx)
}

// metadataSignature reports what happens to a detached repomd.xml signature.
// Republishing rewrites repomd.xml, so a signature that is not replaced is left
// describing a document that no longer exists — which is worse than no
// signature at all, because a client with repo_gpgcheck=1 fails hard.
func (a *analysis) metadataSignature(ctx context.Context) error {
	sig, err := a.r.MetadataSignature(ctx)
	if err != nil {
		return err
	}
	pub := &a.rep.Published
	if sig == nil {
		if a.opt.SignMetadata {
			a.rep.add(Finding{
				Severity: Info, Code: "metadata-signature-added",
				Summary: "repomd.xml is unsigned today and would be signed by " + a.opt.GPGKeyID,
				Detail: "clients configured with repo_gpgcheck=1 would then need this key; clients that are not " +
					"are unaffected.",
			})
		}
		return nil
	}

	pub.SignedMetadata = true
	// A signature this build cannot parse is not an error worth failing on: the
	// report says "an unidentified key" and carries on.
	ids, _ := sign.ArmoredSignatureIssuers(sig)
	pub.MetadataSigners = ids
	signer := "an unidentified key"
	if len(pub.MetadataSigners) > 0 {
		signer = strings.Join(pub.MetadataSigners, ", ")
	}

	if !a.opt.SignMetadata {
		a.rep.add(Finding{
			Severity: Blocking, Code: "metadata-signature-invalidated",
			Summary: "repomd.xml is signed (by " + signer + ") but the republished one would not be",
			Detail: "the republish rewrites repomd.xml and leaves repodata/repomd.xml.asc in place, so the " +
				"signature no longer matches the document. Every client with repo_gpgcheck=1 then fails to refresh " +
				"this repository.",
			Fix: "--sign-metadata with --gpg-key/--gpg-key-id",
		})
		return nil
	}
	if key := strings.ToLower(a.opt.GPGKeyID); key != "" && len(pub.MetadataSigners) > 0 && !sign.SameSigner(pub.MetadataSigners, sign.KeyAndSubkeyIDs(key)) {
		a.rep.add(Finding{
			Severity: Blocking, Code: "metadata-signer-changed",
			Summary: "repomd.xml is signed by " + signer + ", but the republish would sign it with " + a.opt.GPGKeyID,
			Detail: "clients that have only the current key imported reject the new signature, which fails the " +
				"repository refresh outright when repo_gpgcheck=1.",
			Fix: "sign with the repository's existing key, or distribute the new public key to clients first",
		})
	}
	return nil
}

// packageSignatures downloads a sample of packages and reads their signatures,
// which is the only way to learn who signed a repository this tool has not
// published before. Sampling is bounded because each package is a full
// download.
func (a *analysis) packageSignatures(ctx context.Context) error {
	pkgs := a.published
	if a.opt.SamplePackages <= 0 || len(pkgs) == 0 {
		return nil
	}
	n := min(a.opt.SamplePackages, len(pkgs))
	sample := pkgs[:n]
	if a.opt.Progress != nil && !backend.IsLocal(a.r.Backend()) {
		var bytes int64
		for _, p := range sample {
			bytes += p.SizePackage
		}
		a.opt.Progress(fmt.Sprintf("reading the signature of %s (~%s to download)",
			plural(n, "package", "packages"), humanBytes(bytes)))
	}

	var ids []string
	unsigned, noV4 := 0, 0
	for _, p := range sample {
		local, err := a.r.GetToFile(ctx, p.Location)
		if errors.Is(err, backend.ErrNotExist) {
			continue // already reported as a missing package
		}
		if err != nil {
			return fmt.Errorf("sampling %s: %w", p.Location, err)
		}
		info, err := sign.InspectPackage(local)
		removeFile(local)
		if err != nil {
			return fmt.Errorf("sampling %s: %w", p.Location, err)
		}
		a.rep.Published.SampledPackages++
		switch {
		case !info.Signed:
			unsigned++
		case !info.HeaderV4:
			noV4++
		}
		ids = append(ids, info.KeyIDs...)
	}
	a.rep.Published.PackageSigners = uniqueSorted(ids)
	a.packageSignatureFindings(unsigned, noV4)
	return nil
}

// packageSignatureFindings interprets what the sampled packages carry.
func (a *analysis) packageSignatureFindings(unsigned, noV4 int) {
	sampled := a.rep.Published.SampledPackages
	signers := a.rep.Published.PackageSigners
	switch {
	case sampled == 0:
		return
	case unsigned == sampled:
		a.rep.add(Finding{
			Severity: Info, Code: "packages-unsigned",
			Summary: fmt.Sprintf("every sampled package is unsigned (%s inspected)",
				plural(sampled, "package", "packages")),
			Detail: "nothing here depends on a signing key, so a takeover cannot invalidate one.",
		})
	case noV4 > 0:
		a.rep.add(Finding{
			Severity: Advisory, Code: "packages-without-v4-signature",
			Summary: fmt.Sprintf("%d of %d sampled packages carry no legacy v4 (RSAHEADER) signature",
				noV4, sampled),
			Detail: "rpm 4.14 and 4.16 — RHEL 8 and 9 — read only that signature, so those releases treat these " +
				"packages as unsigned. This is a property of the packages, not of the takeover.",
			Fix: "rebuild --resign-packages --signature-format v4 to add it",
		})
	}
	if len(signers) == 0 || !a.opt.SignPackages || a.opt.GPGKeyID == "" {
		return
	}
	if !sign.SameSigner(signers, sign.KeyAndSubkeyIDs(strings.ToLower(a.opt.GPGKeyID))) {
		a.rep.add(Finding{
			Severity: Advisory, Code: "package-signer-changed",
			Summary: fmt.Sprintf("the published packages are signed by %s, but --sign-packages would sign new ones with %s",
				strings.Join(signers, ", "), a.opt.GPGKeyID),
			Detail: "the repository would then hold packages signed by two different keys, and a client that trusts " +
				"only one of them cannot install from all of it. The existing packages are not re-signed by a takeover.",
			Fix: "sign with the repository's existing key, or rebuild --resign-packages to move the whole repository to the new one",
		})
	}
}

// reconcile applies the requested changes to the in-memory index, exactly as
// rebuild would, so the plan below describes the real outcome.
func (a *analysis) reconcile(ctx context.Context) error {
	ch := &a.rep.Changes
	ch.PackagesBefore = a.r.Index().Len()

	if a.opt.FromPackages {
		if n, size := a.r.RefreshEstimate(); n > 0 && a.opt.Progress != nil {
			a.opt.Progress(fmt.Sprintf("re-reading %s (~%s to download)",
				plural(n, "package", "packages"), humanBytes(size)))
		}
		res, err := a.r.RefreshFromPackages(ctx, repo.RefreshOptions{})
		if err != nil {
			return err
		}
		ch.Corrected = a.refreshFindings(res)
	} else {
		a.rep.add(Finding{
			Severity: Advisory, Code: "metadata-unchecked",
			Summary: "the published metadata is being taken at its word",
			Detail: "without --from-packages the packages are not read, so this analysis cannot tell whether the " +
				"metadata describes the files that are actually stored. A package republished over another under the " +
				"same name is the usual way they diverge.",
			Fix: "--from-packages",
		})
	}

	if a.opt.PruneOlder {
		rep := a.r.PruneOlderVersions()
		ch.Pruned = len(rep.Removed)
		var items []string
		for _, p := range rep.Removed {
			items = append(items, p.NEVRA())
		}
		if len(items) > 0 {
			a.rep.add(Finding{
				Severity: Advisory, Code: "prune-older",
				Summary: fmt.Sprintf("--prune-older would drop %s", plural(len(items), "superseded version", "superseded versions")),
				Detail:  "their RPM files are deleted when the new metadata is published.",
				Items:   truncate(items, maxItems),
			})
		}
		for _, b := range rep.Kept {
			a.rep.add(Finding{
				Severity: Advisory, Code: "prune-kept",
				Summary: fmt.Sprintf("%s is kept although it is superseded: %s requires %s",
					b.Provider.NEVRA(), b.Dependent.NEVRA(), b.Requires.Constraint()),
				Fix: "--prune-break-deps to drop it anyway",
			})
		}
		for _, b := range rep.Broken {
			a.rep.add(Finding{
				Severity: Blocking, Code: "prune-broken",
				Summary: fmt.Sprintf("--prune-break-deps would drop %s though %s requires %s",
					b.Provider.NEVRA(), b.Dependent.NEVRA(), b.Requires.Constraint()),
				Detail: "the dependency becomes unsatisfiable for clients of this repository.",
			})
		}
	}

	if a.opt.Relocate {
		ch.Relocated = a.r.RelocateAll(a.opt.LocationPrefix)
	}
	ch.PackagesAfter = a.r.Index().Len()
	return nil
}

// refreshFindings turns the result of re-reading the packages into findings,
// returning the number of packages whose metadata the re-read actually
// corrected.
func (a *analysis) refreshFindings(res *repo.RefreshResult) int {
	mismatched, unproven := a.packageFileMismatches()
	if len(mismatched) > 0 {
		a.rep.add(Finding{
			Severity: Blocking, Code: "metadata-does-not-match-packages",
			Summary: fmt.Sprintf("%s described by metadata that does not match the stored file",
				plural(len(mismatched), "package is", "packages are")),
			Detail: "the stored RPM differs in size or checksum from what the metadata records, so clients " +
				"downloading it see a size or checksum mismatch. Taking the repository over with --from-packages " +
				"corrects the metadata; this is the repository's existing state, not damage a takeover would do.",
			Fix:   "rebuild --from-packages, which republishes the metadata this analysis just derived",
			Items: truncate(mismatched, maxItems),
		})
	}
	if len(unproven) > 0 {
		a.rep.add(Finding{
			Severity: Info, Code: "checksum-not-comparable",
			Summary: fmt.Sprintf("%s indexed with a checksum this tool does not compute; only the size could be compared",
				plural(len(unproven), "package is", "packages are")),
			Detail: "each of these files is the size the metadata records, but the published pkgid is in another " +
				"algorithm, so the re-read cannot confirm the content byte for byte. The regenerated metadata " +
				"describes the file that is actually stored either way.",
			Items: truncate(unproven, maxItems),
		})
	}
	if len(res.Missing) > 0 {
		a.rep.add(Finding{
			Severity: Blocking, Code: "missing-packages",
			Summary: fmt.Sprintf("%s referenced by the metadata but not stored here", plural(len(res.Missing), "package is", "packages are")),
			Detail:  "their entries are left exactly as published, so the broken references survive a republish.",
			Items:   truncate(res.Missing, maxItems),
		})
	}
	if len(res.Unreadable) > 0 {
		a.rep.add(Finding{
			Severity: Blocking, Code: "unreadable-packages",
			Summary: fmt.Sprintf("%s stored but could not be read as an RPM", plural(len(res.Unreadable), "file is", "files are")),
			Items:   truncate(res.Unreadable, maxItems),
		})
	}
	if res.Duplicates > 0 {
		a.rep.add(Finding{
			Severity: Advisory, Code: "duplicate-records",
			Summary: fmt.Sprintf("%s naming a file another record already describes", plural(res.Duplicates, "metadata record is", "metadata records are")),
			Detail:  "the stale records are dropped; no package file is deleted, since another record still names each one.",
		})
	}
	for _, c := range res.Conflicts {
		a.rep.add(Finding{
			Severity: Advisory, Code: "identical-packages",
			Summary: c,
			Detail: "the index is keyed by checksum and cannot hold both, and dropping either would orphan its " +
				"RPM, so both entries are left exactly as published.",
			Fix: "remove one of the two files if the duplication is unintended",
		})
	}
	if a.opt.ChangelogLimit > 0 {
		a.rep.add(Finding{
			Severity: Info, Code: "changelog-limit",
			Summary: fmt.Sprintf("other.xml would keep only the %d most recent changelog entries per package", a.opt.ChangelogLimit),
			Detail:  "re-reading the RPMs re-derives the changelogs, and this limit is applied as they are read.",
			Fix:     "--changelog-limit 0 to keep every entry",
		})
	}
	return len(mismatched)
}

// packageFileMismatches compares the metadata regenerated from the RPMs against
// the metadata as published, and reports which packages the files disagree
// with. A differing pkgid does not settle it on its own: a repository indexed
// with another checksum algorithm produces a different pkgid for every package
// however faithful its metadata is, so those are reported separately as
// compared by size alone rather than as corrupt.
func (a *analysis) packageFileMismatches() (mismatched, unproven []string) {
	current := make(map[string]*repodata.Package, a.r.Index().Len())
	for _, p := range a.r.Index().Packages() {
		current[p.NEVRA()] = p
	}
	for _, old := range a.published {
		now, ok := current[old.NEVRA()]
		if !ok {
			continue // reported elsewhere (missing, unreadable or pruned)
		}
		sameSize := old.SizePackage == now.SizePackage
		sameAlgo := strings.EqualFold(old.ChecksumType, now.ChecksumType)
		switch {
		case !sameSize:
			mismatched = append(mismatched, fmt.Sprintf("%s (metadata says %s, the file is %s)",
				old.NEVRA(), humanBytes(old.SizePackage), humanBytes(now.SizePackage)))
		case sameAlgo && !strings.EqualFold(old.PkgID, now.PkgID):
			mismatched = append(mismatched, fmt.Sprintf("%s (checksum %s, the file hashes to %s)",
				old.NEVRA(), shortSum(old.PkgID), shortSum(now.PkgID)))
		case !sameAlgo:
			unproven = append(unproven, fmt.Sprintf("%s (%s)", old.NEVRA(), strings.ToLower(old.ChecksumType)))
		}
	}
	sortStrings(mismatched)
	sortStrings(unproven)
	return mismatched, unproven
}

// plan computes the commit plan the reconciled index would produce and records
// what it would transfer, move and delete.
func (a *analysis) plan(ctx context.Context) error {
	plan, err := a.r.Plan(ctx)
	//nolint:nilerr // a plan that cannot be computed is the answer, reported as a finding.
	if err != nil {
		// A plan that cannot be computed is itself the answer: the usual cause
		// is a relocation on a backend that cannot copy server-side, which
		// would need every package re-uploaded from a local copy nobody has.
		a.rep.add(Finding{
			Severity: Blocking, Code: "plan-failed",
			Summary: "the republish could not be planned: " + err.Error(),
			Detail: "nothing below describes what would be transferred, because the operation cannot be carried " +
				"out as configured.",
		})
		return nil
	}

	ch := &a.rep.Changes
	ch.Uploads = len(plan.Uploads)
	ch.UploadBytes = plan.BytesToUpload
	ch.ServerSideMoves = len(plan.Copies)
	ch.MetadataWritten = plan.MetadataFiles
	ch.SignMetadata = plan.Signed

	moved := map[string]bool{}
	for _, c := range plan.Copies {
		moved[c.From] = true
	}
	for _, href := range plan.ObsoleteMeta {
		ch.DeletedFiles = append(ch.DeletedFiles, href)
		ch.DeletedBytes += a.fileSize(href)
	}
	for _, href := range plan.DeletedRPMs {
		if moved[href] {
			continue // the source of a server-side move, not a loss
		}
		ch.DeletedFiles = append(ch.DeletedFiles, href)
		ch.DeletedBytes += a.fileSize(href)
	}
	sortStrings(ch.DeletedFiles)

	if ch.Relocated > 0 {
		sev, detail := Advisory, "the files are moved server-side, with no re-upload."
		if ch.ServerSideMoves < ch.Relocated {
			sev = Blocking
			detail = "not every move can be made server-side on this backend, so some packages would have to be re-uploaded."
		}
		a.rep.add(Finding{
			Severity: sev, Code: "packages-relocated",
			Summary: fmt.Sprintf("%s would move to %s/",
				plural(ch.Relocated, "package", "packages"), strings.Trim(a.opt.LocationPrefix, "/")),
			Detail: detail + " Clients holding the old metadata keep requesting the old paths until they refresh, " +
				"and the old files are deleted as soon as the new metadata is published.",
		})
	}
	if ch.Uploads > 0 {
		a.rep.add(Finding{
			Severity: Advisory, Code: "uploads-required",
			Summary: fmt.Sprintf("%s would be uploaded (%s)",
				plural(ch.Uploads, "file", "files"), humanBytes(ch.UploadBytes)),
			Detail: "a takeover normally transfers no packages at all, so this is worth understanding before it runs.",
		})
	}
	return nil
}

// fileSize reports a stored file's size: from the backend's listing where there
// is one, and otherwise from what the metadata records about it. A file nothing
// accounts for contributes zero rather than a guess.
func (a *analysis) fileSize(href string) int64 {
	if size, ok := a.sizes[href]; ok {
		return size
	}
	for _, d := range a.r.Repomd().Data {
		if d.Location == href {
			return d.Size
		}
	}
	for _, p := range a.published {
		if p.Location == href {
			return p.SizePackage
		}
	}
	return 0
}

// diff compares the metadata that would be written against the metadata as
// published, field by field, and reports where they differ. It is what turns
// "the metadata is regenerated" into a statement about what actually changes.
func (a *analysis) diff() {
	a.rep.Diff = diffPackages(a.published, a.filelists, a.changelog, a.r.Index().Packages())
}

// dependencies records requirements the repository cannot satisfy from its own
// packages once the reconciliation is applied.
func (a *analysis) dependencies() {
	problems := a.r.CheckDependencies()
	if len(problems) == 0 {
		return
	}
	var items []string
	for _, p := range problems {
		items = append(items, p.String())
	}
	a.rep.Changes.DependencyProblems = items
	a.rep.add(Finding{
		Severity: Advisory, Code: "unmet-dependencies",
		Summary: fmt.Sprintf("%s within the repository would be unmet",
			plural(len(items), "dependency", "dependencies")),
		Detail: "a capability some package in the repository provides is required at a version none of them offers.",
		Items:  truncate(items, maxItems),
	})
}

// proposeConfig builds the createrepo-go.json a takeover would record. It
// starts from any config already present, applies the settings this analysis
// ran with, and infers what it can from the repository itself.
func (a *analysis) proposeConfig() {
	cfg := repoconfig.Config{}
	if a.opt.ExistingConfig != nil {
		cfg = *a.opt.ExistingConfig
	}
	if a.opt.Name != "" {
		cfg.Name = a.opt.Name
	}
	if a.opt.BaseURL != "" {
		cfg.BaseURL = a.opt.BaseURL
	}
	// A repository that tells clients where to fetch its packages from has
	// already stated its own base URL; record it rather than asking again.
	if cfg.BaseURL == "" && len(a.rep.Published.XMLBases) == 1 {
		cfg.BaseURL = a.rep.Published.XMLBases[0]
	}
	cfg.Target = a.opt.Target
	cfg.LocationPrefix = strings.Trim(a.opt.LocationPrefix, "/")
	// Without an explicit --location-prefix, record where the packages already
	// are, so later adds join them instead of starting a second directory.
	if !a.opt.Relocate {
		if dirs := a.rep.Published.PackageDirs; len(dirs) == 1 && dirs[0] != "(repository root)" {
			cfg.LocationPrefix = dirs[0]
		}
	}
	cfg.SignMetadata = a.opt.SignMetadata
	cfg.SignPackages = a.opt.SignPackages
	if a.opt.SignMetadata || a.opt.SignPackages {
		cfg.SignatureFormat = a.opt.SignatureFormat
		if fpr, err := sign.Fingerprint("", a.opt.GPGKeyID); err == nil && fpr != "" {
			cfg.GPGKeyID = fpr
		} else if a.opt.GPGKeyID != "" {
			cfg.GPGKeyID = a.opt.GPGKeyID
		}
	}
	a.rep.Config = cfg
}
