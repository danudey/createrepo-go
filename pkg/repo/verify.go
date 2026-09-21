package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/progress"
	"github.com/danudey/createrepo-go/pkg/repodata"
)

// Problem is one discrepancy found while verifying a published repository.
type Problem struct {
	// Kind groups the finding: "package", "dependency".
	Kind string
	// Subject names what is wrong — a package NEVRA, or a dependency.
	Subject string
	// Location is the repo-relative path involved, when there is one.
	Location string
	// Detail explains the discrepancy.
	Detail string
}

func (p Problem) String() string {
	if p.Location != "" {
		return fmt.Sprintf("%s: %s (%s)", p.Subject, p.Detail, p.Location)
	}
	return fmt.Sprintf("%s: %s", p.Subject, p.Detail)
}

// ChecksumMode selects how thoroughly Verify checks that each RPM's content
// matches the pkgid the metadata records.
type ChecksumMode int

const (
	// ChecksumCheap uses only a checksum the backend can supply without
	// transferring the object. That is a real content hash on local disk and
	// over SSH, but on an object store it is the checksum recorded at upload
	// time, and on plain HTTP there is none at all. Packages whose checksum
	// could not be confirmed from content are counted in
	// VerifyResult.ChecksumUnconfirmed rather than reported as problems.
	ChecksumCheap ChecksumMode = iota

	// ChecksumContent proves every package's checksum from its bytes,
	// downloading it when the backend cannot hash content itself. It is the
	// only mode that detects an object whose content changed after it was
	// published.
	ChecksumContent
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

	conc := opt.Concurrency
	if conc < 1 {
		conc = 1
	}
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, conc)
	)
	for _, p := range pkgs {
		wg.Add(1)
		sem <- struct{}{}
		go func(p *repodata.Package) {
			defer wg.Done()
			defer func() { <-sem }()
			one := r.verifyPackage(ctx, p, opt)

			mu.Lock()
			res.Problems = append(res.Problems, one.problems...)
			res.ChecksumVerified += one.verified
			res.ChecksumRecorded += one.recorded
			res.ChecksumUnconfirmed += one.unconfirmed
			res.Downloaded += one.downloaded
			res.BytesRead += one.bytesRead
			mu.Unlock()

			if opt.Progress != nil {
				opt.Progress(p, len(one.problems))
			}
		}(p)
	}
	wg.Wait()

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

	sort.Slice(res.Problems, func(i, j int) bool {
		a, b := res.Problems[i], res.Problems[j]
		if a.Subject != b.Subject {
			return a.Subject < b.Subject
		}
		return a.Detail < b.Detail
	})
	return res, nil
}

// packageVerdict is one package's contribution to a VerifyResult.
type packageVerdict struct {
	problems    []Problem
	verified    int
	recorded    int
	unconfirmed int
	downloaded  int
	bytesRead   int64
}

func (v *packageVerdict) fail(p *repodata.Package, detail string) {
	v.problems = append(v.problems, Problem{
		Kind: "package", Subject: p.NEVRA(), Location: p.Location, Detail: detail,
	})
}

// verifyPackage checks one package's presence, size and checksum.
func (r *Repo) verifyPackage(ctx context.Context, p *repodata.Package, opt VerifyOptions) packageVerdict {
	var v packageVerdict

	fi, err := r.be.Stat(ctx, p.Location)
	switch {
	case errors.Is(err, backend.ErrNotExist):
		v.fail(p, "the RPM the metadata references is missing")
		r.opt.Tracker.Skip(1, p.SizePackage)
		return v
	case err != nil:
		v.fail(p, "stat failed: "+err.Error())
		r.opt.Tracker.Skip(1, p.SizePackage)
		return v
	}
	if p.SizePackage > 0 && fi.Size != p.SizePackage {
		v.fail(p, fmt.Sprintf("size %d does not match the %d the metadata records", fi.Size, p.SizePackage))
		// Keep going: the checksum tells the operator whether the file is a
		// different build or a damaged one.
	}

	if p.PkgID == "" {
		v.unconfirmed++
		r.opt.Tracker.Skip(1, p.SizePackage)
		return v
	}

	// Content is authoritative. Use the backend's own hash only when it is
	// computed from the stored bytes; otherwise fall back to reading the object.
	if backend.HashesContent(r.be) {
		hasher := r.be.(backend.RemoteHasher)
		sum, ok, err := hasher.Hash(ctx, p.Location, r.opt.checksumType())
		switch {
		case err != nil && !errors.Is(err, backend.ErrNotExist):
			v.fail(p, "checksum could not be computed: "+err.Error())
			return v
		case ok:
			if !strings.EqualFold(sum, p.PkgID) {
				v.fail(p, fmt.Sprintf("content checksum %s does not match the pkgid %s the metadata records", sum, p.PkgID))
			} else {
				v.verified++
			}
			return v
		}
		// ok == false: the backend could not answer after all; fall through.
	}

	if opt.Checksums != ChecksumContent {
		// Cheap mode: accept a recorded checksum as corroboration, and record
		// honestly when there is nothing to go on.
		if hasher, isHasher := r.be.(backend.RemoteHasher); isHasher {
			sum, ok, err := hasher.Hash(ctx, p.Location, r.opt.checksumType())
			if err != nil && !errors.Is(err, backend.ErrNotExist) {
				v.fail(p, "recorded checksum could not be read: "+err.Error())
				return v
			}
			if ok {
				if !strings.EqualFold(sum, p.PkgID) {
					v.fail(p, fmt.Sprintf("the checksum %s recorded by the store does not match the pkgid %s the metadata records", sum, p.PkgID))
				} else {
					v.recorded++
				}
				return v
			}
		}
		v.unconfirmed++
		return v
	}

	// Content mode against a backend that cannot hash for us: read the object.
	item := r.opt.Tracker.Item(path.Base(p.Location), p.SizePackage)
	sum, n, err := hashObject(ctx, r.be, p.Location, item)
	item.Done()
	v.downloaded++
	v.bytesRead += n
	if err != nil {
		v.fail(p, "reading the RPM to check its checksum: "+err.Error())
		return v
	}
	if p.SizePackage > 0 && n != p.SizePackage {
		v.fail(p, fmt.Sprintf("read %d bytes, but the metadata records a size of %d", n, p.SizePackage))
	}
	if !strings.EqualFold(sum, p.PkgID) {
		v.fail(p, fmt.Sprintf("content checksum %s does not match the pkgid %s the metadata records", sum, p.PkgID))
		return v
	}
	v.verified++
	return v
}

// hashObject streams an object from the backend through sha256, returning the
// hex digest and the number of bytes read. Nothing is buffered: an RPM of any
// size costs one pass and no disk.
func hashObject(ctx context.Context, be backend.Backend, relpath string, item *progress.Item) (string, int64, error) {
	rc, err := be.Get(ctx, relpath)
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, item.Reader(rc))
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
