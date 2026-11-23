package repo

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/danudey/createrepo-go/pkg/apt/aptdata"
	"github.com/danudey/createrepo-go/pkg/apt/debmeta"
	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/xfer"
)

// KindMetadata is the Problem kind for a defect in the Release or an index.
const KindMetadata = "metadata"

// MetadataFile is one file a Release lists, as CheckMetadata found it.
type MetadataFile struct {
	Path string // repo-root-relative
	// OK is false when the file is one of the report's problems; Detail says
	// what was checked or what is wrong. Absent is set for a file the
	// Release lists that is not published but need not be, because another
	// compressed form of the same index is.
	OK     bool
	Absent bool
	Detail string
}

// MetadataReport is what CheckMetadata found.
type MetadataReport struct {
	// Release is the parsed Release, nil when there is none to read.
	Release *aptdata.Release
	// ReleasePath is the file it was read from: InRelease or Release.
	ReleasePath string
	Files       []MetadataFile
	Problems    []Problem
	Warnings    []Problem
	// Cache holds every index read, by path, for Options.MetadataCache.
	Cache map[string][]byte

	packageIndexFailed bool
}

// Usable reports whether the suite's indexes can be loaded.
func (m *MetadataReport) Usable() bool { return m.Release != nil && !m.packageIndexFailed }

// CheckMetadata checks a suite's metadata as apt reads it: the Release
// (InRelease preferred) is present, parses and has not expired, and every
// file it lists in the given components (all when empty) is present with the
// recorded size and every recorded checksum, decompresses, and, for a
// Packages or Sources index, parses. Where the Release lists several
// compressed forms of one index, apt fetches whichever it prefers, so one
// form may be absent as long as another is sound. With Acquire-By-Hash, each
// file's by-hash copy, which is what apt fetches, must be present too. Every
// defect is reported rather than stopping the check.
func CheckMetadata(ctx context.Context, be backend.Backend, suite string, components []string) *MetadataReport {
	rep := &MetadataReport{Cache: map[string][]byte{}}
	problem := func(subject, loc, detail string) {
		rep.Problems = append(rep.Problems, Problem{Kind: KindMetadata, Subject: subject, Location: loc, Detail: detail})
	}

	for _, spec := range []struct {
		path      string
		clearsign bool
	}{{aptdata.InReleasePath(suite), true}, {aptdata.ReleasePath(suite), false}} {
		data, err := xfer.ReadAll(ctx, be, spec.path)
		if errors.Is(err, backend.ErrNotExist) {
			continue
		}
		if err != nil {
			problem(spec.path, "", "it cannot be read: "+err.Error())
			return rep
		}
		if spec.clearsign {
			data = debmeta.StripClearsign(data)
		}
		rel, err := aptdata.ParseRelease(data)
		if err != nil {
			problem(spec.path, "", "it does not parse: "+err.Error())
			return rep
		}
		rep.Release, rep.ReleasePath = rel, spec.path
		break
	}
	if rep.Release == nil {
		problem(aptdata.SuiteDir(suite), "", fmt.Sprintf("neither InRelease nor Release is present: there is no %s suite here", suite))
		return rep
	}
	rel := rep.Release
	if until := rel.ValidUntil(); !until.IsZero() && time.Now().After(until) {
		problem(rep.ReleasePath, "", fmt.Sprintf("it expired at its Valid-Until, %s; apt refuses the repository until it is republished", until.Format(aptdata.DateLayout)))
	}
	if len(rel.Files) == 0 {
		problem(rep.ReleasePath, "", "it lists no index files, so apt sees no packages")
	}

	// Group the compressed forms of each index: apt needs one sound form.
	type member struct {
		f    aptdata.IndexFile
		full string
	}
	groups := map[string][]member{}
	var order []string
	for _, f := range rel.Files {
		component, _, isIndex := parseIndexPath(f.Path)
		if isIndex && !wanted(components, component) {
			continue
		}
		full := path.Join(aptdata.SuiteDir(suite), f.Path)
		key := stripCompression(full)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], member{f, full})
	}
	sort.Strings(order)

	for _, key := range order {
		var results []MetadataFile
		parsedOne := false
		for _, m := range groups[key] {
			res := checkIndexFile(ctx, be, rel, m.f, m.full, rep, !parsedOne)
			if res.OK {
				_, _, isIndex := parseIndexPath(m.f.Path)
				parsedOne = parsedOne || isIndex
			}
			results = append(results, res)
		}
		anySound := false
		for _, r := range results {
			anySound = anySound || r.OK
		}
		for i, r := range results {
			if !r.OK && r.Absent && anySound {
				// Another form serves apt; this one need not be published.
				results[i].OK, results[i].Detail = true, "not published; another compressed form of this index is"
			}
		}
		for _, r := range results {
			rep.Files = append(rep.Files, r)
			if r.OK {
				continue
			}
			problem("index", r.Path, r.Detail)
			if _, _, isIndex := parseIndexPath(strings.TrimPrefix(r.Path, aptdata.SuiteDir(suite)+"/")); isIndex {
				rep.packageIndexFailed = true
			}
		}
	}
	return rep
}

// checkIndexFile checks one file the Release lists. parse says whether a
// Packages or Sources index should be parsed: only one form of each needs to
// be, since they decode to the same stanzas.
func checkIndexFile(ctx context.Context, be backend.Backend, rel *aptdata.Release, f aptdata.IndexFile, full string, rep *MetadataReport, parse bool) MetadataFile {
	out := MetadataFile{Path: full}
	fail := func(format string, a ...any) MetadataFile {
		out.Detail = fmt.Sprintf(format, a...)
		return out
	}
	byHash := ""
	if digest, ok := f.Hashes[aptdata.HashSHA256]; ok && rel.AcquireByHash() {
		byHash = aptdata.ByHashPath(path.Dir(full), aptdata.HashSHA256, digest)
	}

	raw, err := xfer.ReadAll(ctx, be, full)
	if errors.Is(err, backend.ErrNotExist) {
		if byHash == "" {
			out.Absent = true
			return fail("the file the Release lists is missing")
		}
		// apt with Acquire-By-Hash fetches only the by-hash copy.
		raw, err = xfer.ReadAll(ctx, be, byHash)
		if errors.Is(err, backend.ErrNotExist) {
			out.Absent = true
			return fail("the file the Release lists is missing, under its own name and under by-hash")
		}
		byHash = ""
	}
	if err != nil {
		return fail("it cannot be read: %v", err)
	}
	if f.Size > 0 && int64(len(raw)) != f.Size {
		return fail("size %d does not match the %d the Release records", len(raw), f.Size)
	}
	algos := f.Hashes.Algorithms()
	if len(algos) == 0 {
		return fail("the Release records no checksum for it, so apt cannot check it")
	}
	got := aptdata.HashBytes(raw, algos)
	for _, algo := range algos {
		if !strings.EqualFold(got[algo], f.Hashes[algo]) {
			return fail("%s %s does not match the %s the Release records", algo, got[algo], f.Hashes[algo])
		}
	}
	if byHash != "" {
		fi, err := be.Stat(ctx, byHash)
		switch {
		case errors.Is(err, backend.ErrNotExist):
			return fail("the Release says Acquire-By-Hash, but the by-hash copy apt fetches, %s, is missing", byHash)
		case err != nil:
			return fail("its by-hash copy %s cannot be read: %v", byHash, err)
		case fi.Size >= 0 && fi.Size != int64(len(raw)):
			return fail("its by-hash copy %s is %d bytes, not %d", byHash, fi.Size, len(raw))
		}
	}

	component, arch, isIndex := parseIndexPath(f.Path)
	body, err := aptdata.DecompressLimit(f.Path, raw, rel.DecodedLimit(f.Path))
	if err != nil {
		return fail("it does not decompress: %v", err)
	}
	if isIndex && parse {
		var n int
		if arch == aptdata.ArchSource {
			srcs, perr := aptdata.ParseSources(body, component)
			n, err = len(srcs), perr
		} else {
			pkgs, perr := aptdata.ParsePackages(body, component)
			n, err = len(pkgs), perr
		}
		if err != nil {
			return fail("it does not parse: %v", err)
		}
		rep.Cache[full] = raw
		out.OK, out.Detail = true, fmt.Sprintf("%d bytes, checksums match, %d stanza(s)", len(raw), n)
		return out
	}
	if isIndex {
		rep.Cache[full] = raw
	}
	out.OK, out.Detail = true, fmt.Sprintf("%d bytes, checksums match", len(raw))
	return out
}

func wanted(list []string, v string) bool {
	return len(list) == 0 || v == "" || slices.Contains(list, v)
}

// stripCompression drops a compression extension, so the forms of one index
// share a key.
func stripCompression(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".gz", ".xz", ".zst", ".zstd", ".bz2", ".lzma":
		return strings.TrimSuffix(p, path.Ext(p))
	}
	return p
}
