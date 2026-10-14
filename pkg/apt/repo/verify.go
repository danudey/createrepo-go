package repo

import (
	"context"
	"path"
	"sync"

	"github.com/danudey/createrepo-go/internal/parallel"
	"github.com/danudey/createrepo-go/pkg/integrity"

	"github.com/danudey/createrepo-go/pkg/apt/aptdata"
	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/progress"
)

// Problem is one discrepancy found while verifying a published repository.
type Problem = integrity.Problem

// ChecksumMode selects how thoroughly Verify checks that each published file's
// content matches the checksum the indexes record.
type ChecksumMode = integrity.Mode

const (
	// ChecksumCheap uses only a checksum the backend can supply without
	// transferring the object. That is a real content hash on local disk and
	// over SSH, but on an object store it is the checksum recorded at upload
	// time, and on plain HTTP there is none at all. Files whose checksum could
	// not be confirmed from content are counted in
	// VerifyResult.ChecksumUnconfirmed rather than reported as problems.
	ChecksumCheap = integrity.Cheap

	// ChecksumContent proves every file's checksum from its bytes, downloading
	// it when the backend cannot hash content itself. It is the only mode that
	// detects an object whose content changed after it was published.
	ChecksumContent = integrity.Content
)

// VerifyOptions configures Verify.
type VerifyOptions struct {
	// Checksums selects how file checksums are established.
	Checksums ChecksumMode

	// Concurrency bounds how many files are checked at once. Zero means 1. It
	// matters most in ChecksumContent mode against a remote backend, where each
	// file is a separate transfer.
	Concurrency int

	// SkipDependencies omits the intra-repository dependency check.
	SkipDependencies bool

	// Progress, if set, is called once per entry as its check finishes. It may
	// be called from several goroutines at once.
	Progress func(e aptdata.Entry, problems int)

	// Tracker, if set, draws progress bars. They only measure something when the
	// check has to read the files: a backend that hashes content for us
	// finishes each entry in one cheap call.
	Tracker *progress.Tracker
}

// VerifyResult summarizes a verification pass.
type VerifyResult struct {
	// Packages and Sources count the indexed entries.
	Packages int
	Sources  int
	// Files counts the individual published files checked. It exceeds
	// Packages+Sources because a source package owns several files.
	Files int

	// ChecksumVerified counts files whose checksum was proved from content.
	ChecksumVerified int
	// ChecksumRecorded counts files whose checksum was confirmed only against a
	// value the backend recorded at upload time.
	ChecksumRecorded int
	// ChecksumUnconfirmed counts files whose checksum could not be established
	// at all (only their size was checked).
	ChecksumUnconfirmed int
	// Downloaded and BytesRead count the files transferred to hash them.
	Downloaded int
	BytesRead  int64

	Problems []Problem
}

// OK reports whether the repository verified cleanly.
func (r *VerifyResult) OK() bool { return len(r.Problems) == 0 }

// DownloadEstimate reports how many files Verify would have to transfer to
// prove their checksums from content, and how many bytes that is according to
// the indexes. It lets a caller warn before starting a long download. It is
// zero when the backend can hash content on its own.
func (r *Repo) DownloadEstimate() (files int, bytes int64) {
	if backend.HashesContent(r.be) {
		return 0, 0
	}
	for _, e := range r.idx.Entries() {
		files += len(e.Locations())
		bytes += e.TotalBytes()
	}
	return files, bytes
}

// Verify checks that every file the indexes reference is present in the backend
// with the size and checksum they claim, and that the repository's
// intra-repository dependencies are satisfied. It never modifies anything.
func (r *Repo) Verify(ctx context.Context, opt VerifyOptions) (*VerifyResult, error) {
	entries := r.idx.Entries()
	res := &VerifyResult{Packages: r.idx.Len(), Sources: r.idx.SourceLen()}

	// The bar measures bytes only when the files have to be read; otherwise
	// each entry is a cheap call and the item count is the whole story.
	var expectedBytes int64
	if opt.Checksums == ChecksumContent && !backend.HashesContent(r.be) {
		expectedBytes = entryTotalBytes(entries)
	}
	opt.Tracker.Begin("verify", entryFileCount(entries), expectedBytes)
	defer opt.Tracker.End()

	var mu sync.Mutex
	parallel.Each(entries, opt.Concurrency, func(e aptdata.Entry) {
		one := r.verifyEntry(ctx, e, opt)

		mu.Lock()
		res.Problems = append(res.Problems, one.problems...)
		res.Files += one.files
		res.ChecksumVerified += one.tally.Verified
		res.ChecksumRecorded += one.tally.Recorded
		res.ChecksumUnconfirmed += one.tally.Unconfirmed
		res.Downloaded += one.tally.Downloaded
		res.BytesRead += one.tally.BytesRead
		mu.Unlock()

		if opt.Progress != nil {
			opt.Progress(e, len(one.problems))
		}
	})

	if !opt.SkipDependencies {
		for _, dp := range r.CheckDependencies() {
			res.Problems = append(res.Problems, Problem{
				Kind:     "dependency",
				Subject:  dp.Package.ID3(),
				Location: dp.Package.Location(),
				Detail:   dp.String(),
			})
		}
	}

	integrity.SortProblems(res.Problems)
	return res, nil
}

// entryVerdict is one entry's contribution to a VerifyResult.
type entryVerdict struct {
	problems []Problem
	files    int
	tally    integrity.Tally
}

// aptWording names a verify finding in createapt-go's terms.
var aptWording = integrity.Wording{
	Records: "the index",
	Missing: "the file the index references is missing",
	Object:  "the file",
}

// verifyEntry checks every file an entry owns. A binary package owns one; a
// source package owns its .dsc and its tarballs, each of which is checked
// against the size and checksum the Sources index records for it.
func (r *Repo) verifyEntry(ctx context.Context, e aptdata.Entry, opt VerifyOptions) entryVerdict {
	var v entryVerdict
	for _, f := range entryFiles(e) {
		v.files++
		task := opt.Tracker.Item(path.Base(f.location), f.size)
		details, tally := integrity.Check(ctx, r.be,
			integrity.File{Location: f.location, Size: f.size, SHA256: f.sha256},
			opt.Checksums, aptWording, func() *progress.Item { return task })
		task.Done()
		v.tally.Add(tally)
		for _, d := range details {
			v.problems = append(v.problems, Problem{Kind: "package", Subject: e.ID3(), Location: f.location, Detail: d})
		}
	}
	return v
}

// verifiableFile is one published file with what the indexes say about it.
type verifiableFile struct {
	location string
	size     int64
	sha256   string
}

// entryFiles lists an entry's files with their expected size and checksum.
func entryFiles(e aptdata.Entry) []verifiableFile {
	switch t := e.(type) {
	case *aptdata.Package:
		return []verifiableFile{{location: t.Location(), size: t.Size(), sha256: t.ID()}}
	case *aptdata.Source:
		dir := t.Directory()
		files := t.SourceFiles()
		out := make([]verifiableFile, 0, len(files))
		for _, f := range files {
			out = append(out, verifiableFile{location: f.Location(dir), size: f.Size, sha256: f.SHA256})
		}
		return out
	default:
		return nil
	}
}
