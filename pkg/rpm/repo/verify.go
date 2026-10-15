package repo

import (
	"context"
	"path"
	"sync"

	"github.com/danudey/createrepo-go/internal/parallel"
	"github.com/danudey/createrepo-go/pkg/integrity"
	"github.com/danudey/createrepo-go/pkg/progress"

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

	Problems []Problem
}

// OK reports whether the repository verified cleanly.
func (r *VerifyResult) OK() bool { return len(r.Problems) == 0 }

// DownloadEstimate reports how many packages Verify would have to transfer to
// prove their checksums from content, and how many bytes that is according to
// the metadata. It lets a caller warn before starting a long download. It is
// zero when the backend can hash content on its own.
func (r *Repo) DownloadEstimate() (packages int, bytes int64) {
	if backend.HashesContent(r.be) {
		return 0, 0
	}
	for _, p := range r.idx.Packages() {
		packages++
		bytes += p.SizePackage
	}
	return packages, bytes
}

// Verify checks that every package the metadata references is present in the
// backend with the size and checksum the metadata claims, and that the
// repository's intra-repository dependencies are satisfied. It never modifies
// anything.
func (r *Repo) Verify(ctx context.Context, opt VerifyOptions) (*VerifyResult, error) {
	pkgs := r.idx.Packages()
	res := &VerifyResult{Packages: len(pkgs)}

	// Only a content check against a backend that cannot hash for us moves any
	// data, and that is exactly what DownloadEstimate measures.
	if opt.Checksums == ChecksumContent {
		if n, bytes := r.DownloadEstimate(); n > 0 {
			r.opt.Tracker.Begin("download", n, bytes)
			defer r.opt.Tracker.End()
		}
	}

	var mu sync.Mutex
	parallel.Each(pkgs, opt.Concurrency, func(p *repodata.Package) {
		problems, tally := r.verifyPackage(ctx, p, opt)

		mu.Lock()
		res.Problems = append(res.Problems, problems...)
		res.ChecksumVerified += tally.Verified
		res.ChecksumRecorded += tally.Recorded
		res.ChecksumUnconfirmed += tally.Unconfirmed
		res.Downloaded += tally.Downloaded
		res.BytesRead += tally.BytesRead
		mu.Unlock()

		if opt.Progress != nil {
			opt.Progress(p, len(problems))
		}
	})

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
	return res, nil
}

// rpmWording names a verify finding in createrepo-go's terms.
var rpmWording = integrity.Wording{
	Records:  "the metadata",
	SumLabel: "pkgid ",
	Missing:  "the RPM the metadata references is missing",
	Object:   "the RPM",
}

// verifyPackage checks one package's presence, size and checksum.
func (r *Repo) verifyPackage(ctx context.Context, p *repodata.Package, opt VerifyOptions) ([]Problem, integrity.Tally) {
	f := integrity.File{Location: p.Location, Size: p.SizePackage, SHA256: sha256Only(p.PkgID, p.ChecksumType)}
	details, tally := integrity.Check(ctx, r.be, f, opt.Checksums, rpmWording, func() *progress.Item {
		return r.opt.Tracker.Item(path.Base(p.Location), p.SizePackage)
	})
	// The progress display, when there is one, covers the downloads; a package
	// settled without one comes off its total.
	if tally.Downloaded == 0 {
		r.opt.Tracker.Skip(1, p.SizePackage)
	}
	problems := make([]Problem, 0, len(details))
	for _, d := range details {
		problems = append(problems, Problem{Kind: "package", Subject: p.NEVRA(), Location: p.Location, Detail: d})
	}
	return problems, tally
}
