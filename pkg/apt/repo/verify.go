package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"sync"

	"github.com/danudey/createrepo-go/internal/parallel"
	"github.com/danudey/createrepo-go/pkg/integrity"

	"github.com/danudey/createrepo-go/pkg/apt/aptdata"
	"github.com/danudey/createrepo-go/pkg/apt/debmeta"
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

	// SkipPackages checks the dependency graph alone, no published file.
	SkipPackages bool
	// SkipPackageContents does not open the .deb files. Verify otherwise
	// reads each one's control file (a few kilobytes at the start of the
	// archive, or as the file streams past when it is downloaded anyway) and
	// checks it names the package, version and architecture the index does.
	SkipPackageContents bool

	// Names, Arches and Components, when set, limit the entries checked;
	// an Architecture: all package counts as every architecture except
	// "source", which selects source packages. LatestOnly checks only the
	// newest version of each name and architecture. The dependency check
	// always covers the whole suite.
	Names      []string
	Arches     []string
	Components []string
	LatestOnly bool

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
func (r *Repo) DownloadEstimate(opt VerifyOptions) (files int, bytes int64) {
	if backend.HashesContent(r.be) {
		return 0, 0
	}
	for _, e := range r.Selected(opt) {
		files += len(e.Locations())
		bytes += e.TotalBytes()
	}
	return files, bytes
}

// Verify checks that every file the indexes reference is present in the backend
// with the size and checksum they claim, and that the repository's
// intra-repository dependencies are satisfied. It never modifies anything.
func (r *Repo) Verify(ctx context.Context, opt VerifyOptions) (*VerifyResult, error) {
	entries := r.Selected(opt)
	res := &VerifyResult{}
	for _, e := range entries {
		if e.Arch() == aptdata.ArchSource {
			res.Sources++
		} else {
			res.Packages++
		}
	}
	if opt.SkipPackages {
		entries = nil
	}

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

// KindPackage is the Problem kind for a defect in a published package file.
const KindPackage = "package"

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
	pkg, isBinary := e.(*aptdata.Package)
	for _, f := range entryFiles(e) {
		v.files++
		task := opt.Tracker.Item(path.Base(f.location), f.size)
		var control *aptdata.Paragraph
		var controlErr error
		var inspect func(io.Reader) error
		if isBinary && !opt.SkipPackageContents {
			inspect = func(rd io.Reader) error {
				control, controlErr = debmeta.ControlFromReader(rd)
				return nil
			}
		}
		details, tally, streamed := integrity.CheckInspect(ctx, r.be,
			integrity.File{Location: f.location, Size: f.size, Checksum: f.sha256},
			opt.Checksums, aptWording, func() *progress.Item { return task }, inspect)
		task.Done()
		v.tally.Add(tally)
		for _, d := range details {
			v.problems = append(v.problems, Problem{Kind: KindPackage, Subject: e.ID3(), Location: f.location, Detail: d})
		}
		if inspect == nil {
			continue
		}
		if !streamed {
			if err := r.readControl(ctx, f.location, inspect); err != nil {
				if !errors.Is(err, backend.ErrNotExist) {
					v.problems = append(v.problems, Problem{Kind: KindPackage, Subject: e.ID3(), Location: f.location, Detail: "reading the .deb to check its control file: " + err.Error()})
				}
				continue
			}
		}
		for _, d := range controlProblems(pkg, control, controlErr) {
			v.problems = append(v.problems, Problem{Kind: KindPackage, Subject: e.ID3(), Location: f.location, Detail: d})
		}
	}
	return v
}

// readControl reads the start of the .deb at location into inspect: in place
// when it is a local file, and otherwise as much of a download as the
// control file takes, which is a few kilobytes.
func (r *Repo) readControl(ctx context.Context, location string, inspect func(io.Reader) error) error {
	var rc io.ReadCloser
	if lp, ok := backend.LocalPath(r.be, location); ok {
		f, err := os.Open(lp)
		if errors.Is(err, fs.ErrNotExist) {
			return backend.ErrNotExist
		}
		if err != nil {
			return err
		}
		rc = f
	} else {
		var err error
		if rc, err = r.be.Get(ctx, location); err != nil {
			return err
		}
	}
	defer rc.Close()
	return inspect(rc)
}

// controlProblems compares a .deb's control file with its index entry: apt
// installs what the archive says, so an archive that is not the package the
// index describes is a different package under that name.
func controlProblems(p *aptdata.Package, control *aptdata.Paragraph, err error) []string {
	if err != nil {
		return []string{"it is not a readable Debian package: " + err.Error()}
	}
	var out []string
	for _, field := range []string{aptdata.FieldPackage, aptdata.FieldVersion, aptdata.FieldArchitecture} {
		want, got := p.Get(field), control.Get(field)
		if field == aptdata.FieldVersion {
			if aptdata.CompareVersions(want, got) == 0 {
				continue
			}
		} else if want == got {
			continue
		}
		out = append(out, fmt.Sprintf("its control file says %s: %s, but the index says %s", field, got, want))
	}
	return out
}

// Selected returns the entries opt selects for checking (see VerifyOptions).
// An Architecture: all package appears in every architecture's index, so each
// entry is listed once.
func (r *Repo) Selected(opt VerifyOptions) []aptdata.Entry {
	var out []aptdata.Entry
	for _, e := range r.idx.Entries() {
		if len(opt.Names) > 0 && !slices.Contains(opt.Names, e.Name()) {
			continue
		}
		if !wantsArch(opt.Arches, e.Arch()) {
			continue
		}
		if c, ok := e.(interface{ ComponentName() string }); ok && !wanted(opt.Components, c.ComponentName()) {
			continue
		}
		out = append(out, e)
	}
	if !opt.LatestOnly {
		return out
	}
	best := map[string]aptdata.Entry{}
	for _, e := range out {
		if cur, ok := best[e.Key()]; !ok || cur.ParsedVersion().Compare(e.ParsedVersion()) < 0 {
			best[e.Key()] = e
		}
	}
	latest := out[:0]
	for _, e := range out {
		if best[e.Key()] == e {
			latest = append(latest, e)
		}
	}
	return latest
}

func wantsArch(arches []string, arch string) bool {
	if len(arches) == 0 {
		return true
	}
	for _, a := range arches {
		// An Architecture: all package installs on every real architecture.
		if a == arch || (arch == aptdata.ArchAll && a != aptdata.ArchSource) {
			return true
		}
	}
	return false
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
