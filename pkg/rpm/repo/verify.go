package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/danudey/createrepo-go/internal/parallel"
	"github.com/danudey/createrepo-go/pkg/integrity"
	"github.com/danudey/createrepo-go/pkg/progress"
	"github.com/danudey/createrepo-go/pkg/rpm/pkgcheck"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/rpm/repodata"
)

// Problem is one discrepancy found while verifying a published repository.
type Problem = integrity.Problem

// ChecksumMode selects how thoroughly Verify checks that each RPM's content
// matches the pkgid the metadata records.
type ChecksumMode = integrity.Mode

const (
	// ChecksumCheap uses only a checksum the backend can supply without
	// transferring the object. That is a real content hash on local disk and
	// over SSH, but on an object store it is the checksum recorded at upload
	// time, and on plain HTTP there is none at all. Packages whose checksum
	// could not be confirmed from content are counted in
	// VerifyResult.ChecksumUnconfirmed rather than reported as problems.
	ChecksumCheap = integrity.Cheap

	// ChecksumContent proves every package's checksum from its bytes,
	// downloading it when the backend cannot hash content itself. It is the
	// only mode that detects an object whose content changed after it was
	// published.
	ChecksumContent = integrity.Content
)

// Kinds of Problem Verify reports, besides the dependency check's
// "dependency".
const (
	KindPackage   = "package"
	KindSignature = "signature"
	KindMetadata  = "metadata"
	// KindCompat is a sound package the target release cannot install.
	KindCompat = "compatibility"
)

// repomdSigPath is the detached signature of repomd.xml.
const repomdSigPath = repomdPath + ".asc"

// VerifyOptions configures Verify.
type VerifyOptions struct {
	// Checksums selects how package checksums are established.
	Checksums ChecksumMode

	// Concurrency bounds how many packages are checked at once. Zero means 1.
	// It matters most in ChecksumContent mode against a remote backend, where
	// each package is a separate transfer.
	Concurrency int

	// SkipDependencies omits the intra-repository dependency check.
	SkipDependencies bool

	// SkipPackages checks the metadata alone: repomd.xml's signature and the
	// dependency graph, but no package file at all.
	SkipPackages bool

	// SkipPackageContents omits reading the packages themselves: their
	// digests and signatures go unchecked, and so do the signature rules
	// below. Verify otherwise reads every package's header, and its payload
	// too wherever the whole file is read anyway (local disk, or
	// ChecksumContent against a backend that cannot hash in place).
	SkipPackageContents bool

	// Keyring holds the trusted public keys signatures are verified against.
	// Without it signatures are checked for integrity only: that they match
	// what they sign, not who made them.
	Keyring openpgp.EntityList

	// Policy is the release whose rpm the package signatures are judged as:
	// which signature tags it reads and what its crypto policy refuses.
	Policy pkgcheck.Policy

	// ExpectedKeyIDs are the long key ids (primary and subkeys) of the key
	// the repository records as its signing key. Any signature by another key
	// is a problem, trusted or not.
	ExpectedKeyIDs []string

	// ExpectedKeyIncomplete says ExpectedKeyIDs may lack the key's subkeys
	// (the key itself was not available to expand). A signature by another
	// key id might then be a subkey's, so it is a warning, not a problem.
	ExpectedKeyIncomplete bool

	// RequireTrusted makes every signature that is present have to verify
	// against Keyring. An intact signature by an unknown key is otherwise
	// accepted.
	RequireTrusted bool

	// PublishedKeys is the public key the repository itself names as the one
	// its users install (its recorded gpg_key_url), from PublishedKeySource.
	// The repository chose it, so it is never trusted: it does not count
	// towards Keyring. It is checked for consistency instead. repomd.xml's
	// signature must verify against it, and every package signature must be
	// by a key it contains, because a signature it cannot verify is one the
	// repository's users cannot verify either.
	PublishedKeys      openpgp.EntityList
	PublishedKeySource string

	// RequireTrustedMetadata is RequireTrusted for repomd.xml's signature
	// alone, as a client with repo_gpgcheck=1 but gpgcheck=0 requires.
	RequireTrustedMetadata bool

	// RequireSignedPackages makes every package have to carry a signature
	// the policy's release can read and accepts.
	RequireSignedPackages bool

	// RequireSignedMetadata makes repomd.xml have to carry a detached
	// signature.
	RequireSignedMetadata bool

	// Names and Arches, when set, limit the packages checked to those names
	// and architectures; noarch counts as every architecture. LatestOnly
	// checks only the newest version of each name and architecture. The
	// dependency check always covers the whole repository.
	Names      []string
	Arches     []string
	LatestOnly bool

	// Progress, if set, is called once per package as its check finishes. It
	// may be called from several goroutines at once.
	Progress func(p *repodata.Package, problems int)
}

// VerifyResult summarizes a verification pass.
type VerifyResult struct {
	Packages int
	// ChecksumVerified counts packages whose checksum was proved from content.
	ChecksumVerified int
	// ChecksumRecorded counts packages whose checksum was confirmed only
	// against a value the backend recorded at upload time.
	ChecksumRecorded int
	// ChecksumUnconfirmed counts packages whose checksum could not be
	// established at all (only their size was checked).
	ChecksumUnconfirmed int
	// Downloaded and BytesRead count the packages transferred to hash them.
	Downloaded int
	BytesRead  int64

	// HeadersChecked counts packages whose header digests and signatures
	// were checked; PayloadsChecked those whose payload digests were too.
	HeadersChecked  int
	PayloadsChecked int

	// Signers maps each key that signs packages (a fingerprint when a trusted
	// key verified it, otherwise the issuer's key id) to how many packages it
	// signs. Unsigned counts packages with no signature the policy's release
	// accepts.
	Signers  map[string]int
	Unsigned int
	// Trusted lists the Signers entries verified against the keyring.
	Trusted map[string]bool

	// Metadata describes repomd.xml's signature.
	Metadata MetadataSignature

	// Problems make the repository fail verification; Warnings do not.
	Problems []Problem
	Warnings []Problem
}

// MetadataSignature describes the detached signature of repomd.xml.
type MetadataSignature struct {
	// Present reports whether repomd.xml.asc exists.
	Present    bool
	Signatures []pkgcheck.Signature
}

// OK reports whether the repository verified cleanly.
func (r *VerifyResult) OK() bool { return len(r.Problems) == 0 }

// DownloadEstimate reports how many packages Verify would have to transfer to
// prove their checksums from content, and how many bytes that is according to
// the metadata. It lets a caller warn before starting a long download. It is
// zero when the backend can hash content on its own.
func (r *Repo) DownloadEstimate(opt VerifyOptions) (packages int, bytes int64) {
	inPlace := backend.HashesContent(r.be)
	for _, p := range r.Selected(opt) {
		// Only sha256 can be hashed in place; any other pkgid type is read.
		if inPlace && integrity.IsSHA256(p.ChecksumType) {
			continue
		}
		packages++
		bytes += p.SizePackage
	}
	return packages, bytes
}

// Selected returns the packages opt selects for checking: those of opt.Names
// and opt.Arches (noarch always counts as one of them), and with
// opt.LatestOnly only the newest version of each name and architecture.
func (r *Repo) Selected(opt VerifyOptions) []*repodata.Package {
	names, arches := toSet(opt.Names), toSet(opt.Arches)
	var out []*repodata.Package
	for _, p := range r.idx.Packages() {
		if len(names) > 0 && !names[p.Name] {
			continue
		}
		if len(arches) > 0 && !arches[p.Arch] && p.Arch != "noarch" {
			continue
		}
		out = append(out, p)
	}
	if !opt.LatestOnly {
		return out
	}
	type key struct{ name, arch string }
	best := map[key]*repodata.Package{}
	var order []key
	for _, p := range out {
		k := key{p.Name, p.Arch}
		cur, ok := best[k]
		switch {
		case !ok:
			best[k] = p
			order = append(order, k)
		case repodata.CompareEVR(p.Epoch, p.Version, p.Release, cur.Epoch, cur.Version, cur.Release) > 0:
			best[k] = p
		}
	}
	latest := make([]*repodata.Package, 0, len(order))
	for _, k := range order {
		latest = append(latest, best[k])
	}
	return latest
}

func toSet(items []string) map[string]bool {
	if len(items) == 0 {
		return nil
	}
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

// checkedPackage is one package and what reading it found.
type checkedPackage struct {
	pkg *repodata.Package
	res *pkgcheck.Result // nil when the package was not read
}

// Verify checks that every package the metadata references is present in the
// backend with the size and checksum the metadata claims, that each package is
// intact and its signatures sound, that the packages and repomd.xml are signed
// as opt requires, and that the repository's intra-repository dependencies
// are satisfied. It never modifies anything.
func (r *Repo) Verify(ctx context.Context, opt VerifyOptions) (*VerifyResult, error) {
	pkgs := r.Selected(opt)
	res := &VerifyResult{Packages: len(pkgs), Signers: map[string]int{}, Trusted: map[string]bool{}}
	if opt.SkipPackages {
		pkgs, opt.SkipPackageContents = nil, true
	}

	// Only a content check against a backend that cannot hash for us moves any
	// data, and that is exactly what DownloadEstimate measures.
	if opt.Checksums == ChecksumContent {
		if n, bytes := r.DownloadEstimate(opt); n > 0 {
			r.opt.Tracker.Begin("download", n, bytes)
			defer r.opt.Tracker.End()
		}
	}

	var mu sync.Mutex
	checked := make([]checkedPackage, 0, len(pkgs))
	parallel.Each(pkgs, opt.Concurrency, func(p *repodata.Package) {
		problems, tally, inspected := r.verifyPackage(ctx, p, opt)

		mu.Lock()
		res.Problems = append(res.Problems, problems...)
		res.ChecksumVerified += tally.Verified
		res.ChecksumRecorded += tally.Recorded
		res.ChecksumUnconfirmed += tally.Unconfirmed
		res.Downloaded += tally.Downloaded
		res.BytesRead += tally.BytesRead
		if inspected != nil {
			res.HeadersChecked++
			if inspected.PayloadChecked {
				res.PayloadsChecked++
			}
		}
		checked = append(checked, checkedPackage{pkg: p, res: inspected})
		mu.Unlock()

		if opt.Progress != nil {
			opt.Progress(p, len(problems))
		}
	})

	if !opt.SkipPackageContents {
		r.checkPackageSigners(checked, opt, res)
	}
	if err := r.checkMetadataSignature(ctx, opt, res); err != nil {
		return nil, err
	}

	if !opt.SkipDependencies {
		for _, dp := range r.CheckDependencies() {
			res.Problems = append(res.Problems, Problem{
				Kind:     "dependency",
				Subject:  dp.Package.NEVRA(),
				Location: dp.Package.Location,
				Detail:   dp.String(),
			})
		}
	}

	integrity.SortProblems(res.Problems)
	integrity.SortProblems(res.Warnings)
	return res, nil
}

// rpmWording names a verify finding in createrepo-go's terms.
var rpmWording = integrity.Wording{
	Records:  "the metadata",
	SumLabel: "pkgid ",
	Missing:  "the RPM the metadata references is missing",
	Object:   "the RPM",
}

// verifyPackage checks one package's presence, size and checksum, then reads
// it to check its digests and signatures. It reads as little as it can: the
// whole file where it is local or being downloaded for its checksum anyway,
// and otherwise just the header, which is where the signatures are.
func (r *Repo) verifyPackage(ctx context.Context, p *repodata.Package, opt VerifyOptions) ([]Problem, integrity.Tally, *pkgcheck.Result) {
	f := integrity.File{Location: p.Location, Size: p.SizePackage, Checksum: p.PkgID, Algo: p.ChecksumType}
	check := pkgcheck.Options{Keyring: opt.Keyring, Policy: opt.Policy}

	var inspected *pkgcheck.Result
	var inspect func(io.Reader) error
	if !opt.SkipPackageContents {
		inspect = func(rd io.Reader) error {
			res, err := pkgcheck.Check(rd, check)
			inspected = res
			return err
		}
	}
	details, tally, streamed := integrity.CheckInspect(ctx, r.be, f, opt.Checksums, rpmWording, func() *progress.Item {
		return r.opt.Tracker.Item(path.Base(p.Location), p.SizePackage)
	}, inspect)
	// The progress display, when there is one, covers the downloads; a package
	// settled without one comes off its total.
	if tally.Downloaded == 0 {
		r.opt.Tracker.Skip(1, p.SizePackage)
	}
	problems := make([]Problem, 0, len(details))
	for _, d := range details {
		problems = append(problems, Problem{Kind: KindPackage, Subject: p.NEVRA(), Location: p.Location, Detail: d})
	}

	if inspect != nil && !streamed {
		var err error
		inspected, err = r.readPackage(ctx, p.Location, check)
		if err != nil && !errors.Is(err, backend.ErrNotExist) {
			problems = append(problems, Problem{
				Kind: KindPackage, Subject: p.NEVRA(), Location: p.Location,
				Detail: "reading the RPM to check its digests and signatures: " + err.Error(),
			})
		}
	}
	if inspected != nil {
		for _, d := range inspected.Problems {
			problems = append(problems, Problem{Kind: KindPackage, Subject: p.NEVRA(), Location: p.Location, Detail: d})
		}
		for _, d := range inspected.SignatureProblems {
			problems = append(problems, Problem{Kind: KindSignature, Subject: p.NEVRA(), Location: p.Location, Detail: d})
		}
		for _, d := range inspected.Incompatible {
			problems = append(problems, Problem{Kind: KindCompat, Subject: p.NEVRA(), Location: p.Location, Detail: d})
		}
		for _, d := range headerMismatches(p, inspected.Header) {
			problems = append(problems, Problem{Kind: KindPackage, Subject: p.NEVRA(), Location: p.Location, Detail: d})
		}
	}
	return problems, tally, inspected
}

// headerMismatches compares what an RPM's header says with what primary.xml
// says about it. dnf resolves dependencies with the metadata and installs the
// header, so a package that differs is not the one dnf chose; and dnf reads
// header-range to fetch the header alone.
func headerMismatches(p *repodata.Package, h *pkgcheck.HeaderInfo) []string {
	if h == nil {
		return nil
	}
	arch := h.Arch
	if h.Source && (p.Arch == "src" || p.Arch == "nosrc") {
		arch = p.Arch
	}
	hdr := fmt.Sprintf("%s-%d:%s-%s.%s", h.Name, h.Epoch, h.Version, h.Release, arch)
	meta := fmt.Sprintf("%s-%d:%s-%s.%s", p.Name, p.Epoch, p.Version, p.Release, p.Arch)
	var out []string
	if hdr != meta {
		out = append(out, fmt.Sprintf("its header says it is %s, but the metadata says %s; the metadata was not generated from this file", hdr, meta))
	}
	if p.HeaderEnd > 0 && (int64(p.HeaderStart) != h.Start || int64(p.HeaderEnd) != h.End) {
		out = append(out, fmt.Sprintf("its header occupies bytes %d-%d, but the metadata's header-range says %d-%d", h.Start, h.End, p.HeaderStart, p.HeaderEnd))
	}
	return out
}

// readPackage checks the package at location: in full when it is a local
// file, and otherwise its header only, which costs a few kilobytes rather
// than the package. A missing package yields backend.ErrNotExist; the
// presence check has already reported it.
func (r *Repo) readPackage(ctx context.Context, location string, opt pkgcheck.Options) (*pkgcheck.Result, error) {
	if lp, ok := backend.LocalPath(r.be, location); ok {
		res, err := pkgcheck.CheckFile(lp, opt)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, backend.ErrNotExist
		}
		return res, err
	}
	rc, err := r.be.Get(ctx, location)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	opt.HeaderOnly = true
	return pkgcheck.Check(rc, opt)
}

// checkPackageSigners applies the repository-wide signature rules: every
// package signed by one key, the key the repository records, and — when
// required — signed at all, by a trusted key.
func (r *Repo) checkPackageSigners(checked []checkedPackage, opt VerifyOptions, res *VerifyResult) {
	expected := make(map[string]bool, len(opt.ExpectedKeyIDs))
	for _, id := range opt.ExpectedKeyIDs {
		expected[strings.ToLower(id)] = true
	}
	release := opt.Policy.Target
	if release == "" {
		release = "the target release"
	}

	identity := map[*repodata.Package]string{}
	var unsigned []*repodata.Package
	for _, c := range checked {
		if c.res == nil {
			continue
		}
		p := c.pkg
		add := func(detail string, warn bool) {
			pr := Problem{Kind: KindSignature, Subject: p.NEVRA(), Location: p.Location, Detail: detail}
			if warn {
				res.Warnings = append(res.Warnings, pr)
			} else {
				res.Problems = append(res.Problems, pr)
			}
		}

		var signers []string
		var unreadable []string
		for _, s := range c.res.Signatures {
			if !s.Counts() {
				if !s.Readable && s.Status != pkgcheck.StatusBad {
					unreadable = append(unreadable, s.Tag)
				}
				continue
			}
			if !slices.Contains(signers, s.Signer()) {
				signers = append(signers, s.Signer())
			}
			if s.Status == pkgcheck.StatusGood {
				res.Trusted[s.Signer()] = true
			}
			if opt.RequireTrusted && s.Status == pkgcheck.StatusUntrusted {
				add(untrustedDetail("the "+s.Tag+" signature", s.Signer(), expected), false)
			}
			if d := unpublishedDetail("the "+s.Tag+" signature", s.KeyID, expected, opt); d != "" {
				add(d, false)
			}
			if len(expected) > 0 && !expected[strings.ToLower(s.KeyID)] && !expected[longKeyID(s.Fingerprint)] {
				add(fmt.Sprintf("the %s signature is by key %s, %s", s.Tag, s.Signer(), notExpected(opt)), opt.ExpectedKeyIncomplete)
			}
		}

		if len(signers) == 0 {
			res.Unsigned++
			unsigned = append(unsigned, p)
			if len(unreadable) > 0 {
				add(fmt.Sprintf("it carries only an %s signature, which %s cannot read, so it installs there as unsigned",
					strings.Join(unreadable, "/"), release), !opt.RequireSignedPackages)
			} else if opt.RequireSignedPackages {
				add("it is not signed", false)
			}
			continue
		}
		sort.Strings(signers)
		id := strings.Join(signers, " + ")
		identity[p] = id
		res.Signers[id]++
	}

	if len(res.Signers) == 0 {
		return
	}
	// The key every package should be signed by: the one the repository
	// records when it signs any, otherwise the one most of them use.
	main := ""
	for id, n := range res.Signers {
		switch {
		case main == "":
			main = id
		case len(expected) > 0 && signerIn(id, expected) != signerIn(main, expected):
			if signerIn(id, expected) {
				main = id
			}
		case n > res.Signers[main] || (n == res.Signers[main] && id < main):
			main = id
		}
	}
	for _, c := range checked {
		id, ok := identity[c.pkg]
		if !ok || id == main {
			continue
		}
		res.Problems = append(res.Problems, Problem{
			Kind: KindSignature, Subject: c.pkg.NEVRA(), Location: c.pkg.Location,
			Detail: fmt.Sprintf("it is signed by %s, but %d other package(s) are signed by %s; every package should be signed by the same key",
				id, res.Signers[main], main),
		})
	}
	if opt.RequireSignedPackages {
		return // already reported each one as unsigned
	}
	for _, p := range unsigned {
		res.Problems = append(res.Problems, Problem{
			Kind: KindSignature, Subject: p.NEVRA(), Location: p.Location,
			Detail: fmt.Sprintf("it is not signed, but %d other package(s) are signed by %s; every package should be signed by the same key",
				res.Signers[main], main),
		})
	}
}

// checkMetadataSignature checks repomd.xml.asc, when there is one, and
// whether there has to be.
func (r *Repo) checkMetadataSignature(ctx context.Context, opt VerifyOptions, res *VerifyResult) error {
	if r.raw == nil {
		return nil
	}
	add := func(detail string) {
		res.Problems = append(res.Problems, Problem{Kind: KindMetadata, Subject: repomdPath, Location: repomdSigPath, Detail: detail})
	}
	data, err := r.getAll(ctx, repomdSigPath)
	if errors.Is(err, backend.ErrNotExist) {
		if opt.RequireSignedMetadata {
			add("repomd.xml is not signed: there is no repomd.xml.asc")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", repomdSigPath, err)
	}
	res.Metadata.Present = true

	// dnf checks repomd.xml.asc with GnuPG, not rpm, so no release's rpm
	// policy applies to it.
	sigs, detachedErr := pkgcheck.CheckDetached(r.raw, data, opt.Keyring, pkgcheck.Policy{})
	if detachedErr != nil {
		// A defect in the repository, not a failure to check it; sigs is
		// empty, so the checks below find nothing more.
		add("repomd.xml.asc does not hold a usable signature: " + detachedErr.Error())
	}
	res.Metadata.Signatures = sigs
	expected := make(map[string]bool, len(opt.ExpectedKeyIDs))
	for _, id := range opt.ExpectedKeyIDs {
		expected[strings.ToLower(id)] = true
	}
	if len(opt.PublishedKeys) > 0 && detachedErr == nil {
		published, _ := pkgcheck.CheckDetached(r.raw, data, opt.PublishedKeys, pkgcheck.Policy{})
		for _, s := range published {
			if s.Status == pkgcheck.StatusGood {
				continue
			}
			d := unpublishedDetail("the repomd.xml signature", s.KeyID, expected, opt)
			if d == "" {
				d = fmt.Sprintf("the public key at %s does not verify the repomd.xml signature: %s", opt.PublishedKeySource, s.Detail)
			}
			add(d)
		}
	}
	for _, s := range sigs {
		switch {
		case s.Status == pkgcheck.StatusBad:
			add("the repomd.xml signature is bad: " + s.Detail)
			continue
		case s.Status != pkgcheck.StatusGood && s.Status != pkgcheck.StatusUntrusted:
			add(fmt.Sprintf("the repomd.xml signature cannot be checked: %s", s.Detail))
			continue
		case s.Rejected != "":
			add("the repomd.xml signature will be refused: " + s.Rejected)
		}
		if (opt.RequireTrusted || opt.RequireTrustedMetadata) && s.Status == pkgcheck.StatusUntrusted {
			add(untrustedDetail("the repomd.xml signature", s.Signer(), expected))
		}
		if len(expected) > 0 && !expected[strings.ToLower(s.KeyID)] && !expected[longKeyID(s.Fingerprint)] {
			pr := Problem{
				Kind: KindMetadata, Subject: repomdPath, Location: repomdSigPath,
				Detail: fmt.Sprintf("repomd.xml is signed by key %s, %s", s.Signer(), notExpected(opt)),
			}
			if opt.ExpectedKeyIncomplete {
				res.Warnings = append(res.Warnings, pr)
			} else {
				res.Problems = append(res.Problems, pr)
			}
		}
	}
	return nil
}

// untrustedDetail words a signature no trusted key verifies. When the signer
// is one of the repository's own key ids, the trusted key is the repository's
// key minus the part that signs: the classic public key exported before a
// signing subkey was added, or with its subkeys stripped.
func untrustedDetail(what, signer string, expected map[string]bool) string {
	d := fmt.Sprintf("%s is by key %s, which is not a trusted key", what, signer)
	if expected[longKeyID(signer)] {
		d += "; that key belongs to the repository's signing key, so the trusted copy of it lacks that subkey." +
			" Export the key with gpg --armor --export, which includes its subkeys, and publish that"
	}
	return d
}

// unpublishedDetail words a signature by a key the published public key does
// not contain, or returns "" when it contains it (or there is none).
func unpublishedDetail(what, keyID string, expected map[string]bool, opt VerifyOptions) string {
	if len(opt.PublishedKeys) == 0 || keyID == "" {
		return ""
	}
	var id uint64
	if _, err := fmt.Sscanf(keyID, "%x", &id); err == nil && len(opt.PublishedKeys.KeysById(id)) > 0 {
		return ""
	}
	d := fmt.Sprintf("%s is by key %s, which the public key at %s does not contain, so the repository's users cannot verify it", what, keyID, opt.PublishedKeySource)
	if expected[longKeyID(keyID)] {
		d += "; that key is a subkey of the repository's signing key. Export the key with gpg --armor --export, which includes its subkeys, and publish that"
	}
	return d
}

// notExpected words a signature by a key other than the recorded one.
func notExpected(opt VerifyOptions) string {
	if opt.ExpectedKeyIncomplete {
		return "which is not the repository's signing key unless it is one of its subkeys (supply the key with --keyring to tell)"
	}
	return "not the repository's signing key"
}

// signerIn reports whether a signer identity (one or more fingerprints or key
// ids joined by " + ") includes an expected key.
func signerIn(id string, expected map[string]bool) bool {
	for s := range strings.SplitSeq(id, " + ") {
		if expected[longKeyID(s)] {
			return true
		}
	}
	return false
}

// longKeyID reduces a fingerprint to its lowercase 16-hex long key id.
func longKeyID(s string) string {
	s = strings.ToLower(s)
	if len(s) > 16 {
		return s[len(s)-16:]
	}
	return s
}
