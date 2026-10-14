// Package integrity checks that a published file is what a repository's
// metadata says it is: present, the recorded size, and the recorded sha256. It
// is the core of both tools' verify command, which differ only in how they
// enumerate files and in the words their reports use.
package integrity

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/progress"
	"github.com/danudey/createrepo-go/pkg/xfer"
)

// Mode selects how thoroughly a file's checksum is established.
type Mode int

const (
	// Cheap uses only a checksum the backend can supply without transferring
	// the object. That is a real content hash on local disk and over SSH, but
	// on an object store it is the checksum recorded at upload time, and on
	// plain HTTP there is none at all. A file whose checksum could not be
	// confirmed from content is counted as unconfirmed rather than reported as
	// a problem.
	Cheap Mode = iota

	// Content proves every file's checksum from its bytes, downloading it when
	// the backend cannot hash content itself. It is the only mode that detects
	// an object whose content changed after it was published.
	Content
)

// Problem is one discrepancy found while verifying a published repository.
type Problem struct {
	// Kind groups the finding: "package", "dependency".
	Kind string
	// Subject names what is wrong — a package identifier, or a dependency.
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

// SortProblems orders problems by subject, then detail, for a stable report.
func SortProblems(ps []Problem) {
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].Subject != ps[j].Subject {
			return ps[i].Subject < ps[j].Subject
		}
		return ps[i].Detail < ps[j].Detail
	})
}

// File is one published file with what the metadata says about it.
type File struct {
	Location string
	// Size is the recorded size; 0 or less means none is recorded.
	Size int64
	// SHA256 is the recorded checksum, lowercase hex; empty means none.
	SHA256 string
}

// Wording names things in a format's own terms, so a report reads the way the
// rest of that tool does.
type Wording struct {
	// Records is what holds the expectation: "the metadata", "the index".
	Records string
	// SumLabel prefixes a recorded checksum: "pkgid " reads "the pkgid ab12…".
	SumLabel string
	// Missing reports an absent file in full.
	Missing string
	// Object is what is read to hash it: "the RPM", "the file".
	Object string
}

func (w Wording) recorded(sum string) string {
	return fmt.Sprintf("the %s%s %s records", w.SumLabel, sum, w.Records)
}

// Tally counts how each file's checksum was established.
type Tally struct {
	// Verified counts files whose checksum was proved from content.
	Verified int
	// Recorded counts files whose checksum was confirmed only against a value
	// the backend recorded at upload time.
	Recorded int
	// Unconfirmed counts files whose checksum could not be established at all
	// (only their size was checked).
	Unconfirmed int
	// Downloaded and BytesRead count the files transferred to hash them.
	Downloaded int
	BytesRead  int64
}

// Add accumulates o into t.
func (t *Tally) Add(o Tally) {
	t.Verified += o.Verified
	t.Recorded += o.Recorded
	t.Unconfirmed += o.Unconfirmed
	t.Downloaded += o.Downloaded
	t.BytesRead += o.BytesRead
}

// Check establishes f's presence, size and checksum in be, returning a detail
// for each discrepancy and how the checksum was established.
//
// item is called only if the file has to be downloaded, so a caller can
// account for progress only for the transfers that happen; the item it returns
// is finished before Check returns. item may be nil.
func Check(ctx context.Context, be backend.Backend, f File, mode Mode, w Wording, item func() *progress.Item) (details []string, t Tally) {
	fail := func(d string) { details = append(details, d) }

	fi, err := be.Stat(ctx, f.Location)
	switch {
	case errors.Is(err, backend.ErrNotExist):
		fail(w.Missing)
		return details, t
	case err != nil:
		fail("stat failed: " + err.Error())
		return details, t
	}
	if f.Size > 0 && fi.Size != f.Size {
		fail(fmt.Sprintf("size %d does not match the %d %s records", fi.Size, f.Size, w.Records))
		// Keep going: the checksum tells the operator whether the file is a
		// different build or a damaged one.
	}

	if f.SHA256 == "" {
		t.Unconfirmed++
		return details, t
	}

	// Content is authoritative. Use the backend's own hash only when it is
	// computed from the stored bytes; otherwise fall back to reading the object.
	if backend.HashesContent(be) {
		sum, ok, err := be.(backend.RemoteHasher).Hash(ctx, f.Location, backend.AlgoSHA256)
		switch {
		case err != nil && !errors.Is(err, backend.ErrNotExist):
			fail("checksum could not be computed: " + err.Error())
			return details, t
		case ok:
			if !strings.EqualFold(sum, f.SHA256) {
				fail(fmt.Sprintf("content checksum %s does not match %s", sum, w.recorded(f.SHA256)))
			} else {
				t.Verified++
			}
			return details, t
		}
		// ok == false: the backend could not answer after all; fall through.
	}

	if mode != Content {
		// Cheap mode: accept a recorded checksum as corroboration, and record
		// honestly when there is nothing to go on.
		if hasher, isHasher := be.(backend.RemoteHasher); isHasher {
			sum, ok, err := hasher.Hash(ctx, f.Location, backend.AlgoSHA256)
			if err != nil && !errors.Is(err, backend.ErrNotExist) {
				fail("recorded checksum could not be read: " + err.Error())
				return details, t
			}
			if ok {
				if !strings.EqualFold(sum, f.SHA256) {
					fail(fmt.Sprintf("the checksum %s recorded by the store does not match %s", sum, w.recorded(f.SHA256)))
				} else {
					t.Recorded++
				}
				return details, t
			}
		}
		t.Unconfirmed++
		return details, t
	}

	// Content mode against a backend that cannot hash for us: read the object.
	var it *progress.Item
	if item != nil {
		it = item()
	}
	sum, n, err := xfer.HashObject(ctx, be, f.Location, it)
	it.Done()
	t.Downloaded++
	t.BytesRead += n
	if err != nil {
		fail(fmt.Sprintf("reading %s to check its checksum: %v", w.Object, err))
		return details, t
	}
	if f.Size > 0 && n != f.Size {
		fail(fmt.Sprintf("read %d bytes, but %s records a size of %d", n, w.Records, f.Size))
	}
	if !strings.EqualFold(sum, f.SHA256) {
		fail(fmt.Sprintf("content checksum %s does not match %s", sum, w.recorded(f.SHA256)))
		return details, t
	}
	t.Verified++
	return details, t
}
