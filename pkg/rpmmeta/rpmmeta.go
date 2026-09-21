// Package rpmmeta extracts rpm-md repository metadata from RPM package files.
// It wraps internal/rpm (a copy of github.com/cavaliergopher/rpm) for header
// parsing but reads the file list directly from the header tags, because that
// library's Files() helper panics on packages that use the modern FILESIZES64
// tag (5008) instead of the legacy FILESIZES tag (1028).
package rpmmeta

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/danudey/createrepo-go/internal/rpm"
	"github.com/danudey/createrepo-go/pkg/repodata"
)

// rpm header tag identifiers we read directly.
const (
	tagChangelogTime = 1080
	tagChangelogName = 1081
	tagChangelogText = 1082

	tagDirIndexes = 1116
	tagBaseNames  = 1117
	tagDirNames   = 1118
	tagFileModes  = 1030
	tagFileFlags  = 1037
)

// rpm file mode bits (from <sys/stat.h>).
const (
	sIFMT  = 0o170000
	sIFDIR = 0o040000
)

// Options controls metadata extraction.
type Options struct {
	// ChecksumType is the package checksum algorithm. Only "sha256" is
	// currently supported (the dnf default on RHEL 8+).
	ChecksumType string
	// ChangelogLimit caps the number of most-recent changelog entries kept.
	// Zero means keep all.
	ChangelogLimit int
}

// DefaultOptions returns sensible defaults (sha256, last 10 changelogs).
func DefaultOptions() Options {
	return Options{ChecksumType: "sha256", ChangelogLimit: 10}
}

// FromFile parses the RPM at path and returns its repository metadata. The
// package checksum is computed by streaming the file, and the on-disk size and
// mtime are taken from the file's stat info.
func FromFile(path string, opt Options) (*repodata.Package, error) {
	if opt.ChecksumType == "" {
		opt.ChecksumType = "sha256"
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	// Parse the headers, then checksum the whole file in a second pass.
	pkg, err := rpm.Read(f)
	if err != nil {
		return nil, fmt.Errorf("read rpm %s: %w", path, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	sum := hex.EncodeToString(h.Sum(nil))

	out := convert(pkg, opt)
	out.ChecksumType = opt.ChecksumType
	out.PkgID = sum
	out.SizePackage = fi.Size()
	out.FileTime = fi.ModTime().Unix()
	// Default location is the basename at the repo root; callers may override.
	out.Location = baseName(path)
	return out, nil
}

// baseName is filepath.Base restricted to what a location needs. It must be
// filepath, not path: the argument is a local filesystem path, so on Windows
// the separator is a backslash and a path-only split would return the whole
// path and publish a package at "..\..\somewhere\hello.rpm".
func baseName(p string) string {
	return filepath.Base(p)
}

// convert maps a parsed rpm.Package into our metadata model (everything that
// does not require the file's stat info or checksum).
func convert(pkg *rpm.Package, opt Options) *repodata.Package {
	start, end := pkg.HeaderRange()
	p := &repodata.Package{
		Name:        pkg.Name(),
		Arch:        pkg.Architecture(),
		Epoch:       pkg.Epoch(),
		Version:     pkg.Version(),
		Release:     pkg.Release(),
		Summary:     pkg.Summary(),
		Description: pkg.Description(),
		Packager:    pkg.Packager(),
		URL:         pkg.URL(),
		BuildTime:   pkg.BuildTime().Unix(),
		SizeInstall: clampInt64(pkg.Size()),
		SizeArchive: clampInt64(pkg.ArchiveSize()),
		License:     pkg.License(),
		Vendor:      pkg.Vendor(),
		Group:       firstOr(pkg.Groups(), ""),
		BuildHost:   pkg.BuildHost(),
		SourceRPM:   pkg.SourceRPM(),
		HeaderStart: start,
		HeaderEnd:   end,
		Provides:    deps(pkg.Provides(), false),
		Requires:    deps(pkg.Requires(), true),
		Conflicts:   deps(pkg.Conflicts(), false),
		Obsoletes:   deps(pkg.Obsoletes(), false),
		Files:       files(pkg),
		Changelogs:  changelogs(pkg, opt.ChangelogLimit),
	}
	return p
}

// clampInt64 converts a size reported by the rpm header to the signed type
// rpm-md uses. A real package never approaches the limit, but the header is
// attacker-controlled and a bare conversion would wrap to a negative size.
func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

func firstOr(s []string, def string) string {
	if len(s) > 0 {
		return s[0]
	}
	return def
}

// flagString maps the rpm dependency comparison bits to the rpm-md flags
// attribute. It returns "" when no version comparison is expressed.
func flagString(flags int) string {
	const (
		less    = rpm.DepFlagLesser
		greater = rpm.DepFlagGreater
		equal   = rpm.DepFlagEqual
	)
	switch flags & (less | greater | equal) {
	case equal:
		return "EQ"
	case less:
		return "LT"
	case greater:
		return "GT"
	case less | equal:
		return "LE"
	case greater | equal:
		return "GE"
	default:
		return ""
	}
}

// isPre reports whether a requires entry is a pre/scriptlet dependency.
func isPre(flags int) bool {
	const mask = rpm.DepFlagPrereq |
		rpm.DepFlagScriptPre | rpm.DepFlagScriptPost |
		rpm.DepFlagScriptPreUn | rpm.DepFlagScriptPostUn
	return flags&mask != 0
}

// deps converts rpm dependencies into metadata entries, dropping rpmlib()
// pseudo-dependencies (createrepo_c excludes them) and de-duplicating.
func deps(in []rpm.Dependency, requires bool) []repodata.Entry {
	var out []repodata.Entry
	seen := make(map[string]bool)
	for _, d := range in {
		name := d.Name()
		if strings.HasPrefix(name, "rpmlib(") || d.Flags()&rpm.DepFlagRpmlib != 0 {
			continue
		}
		fl := flagString(d.Flags())
		e := repodata.Entry{Name: name, Flags: fl}
		if fl != "" {
			e.Epoch = d.Epoch()
			e.Ver = d.Version()
			e.Rel = d.Release()
		}
		if requires {
			e.Pre = isPre(d.Flags())
		}
		key := fmt.Sprintf("%s|%s|%d|%s|%s|%v", e.Name, e.Flags, e.Epoch, e.Ver, e.Rel, e.Pre)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	return out
}

// files reads the package file list directly from the header tags.
func files(pkg *rpm.Package) []repodata.File {
	base := pkg.Header.GetTag(tagBaseNames).StringSlice()
	if len(base) == 0 {
		return nil
	}
	dirs := pkg.Header.GetTag(tagDirNames).StringSlice()
	ixs := pkg.Header.GetTag(tagDirIndexes).Int64Slice()
	modes := pkg.Header.GetTag(tagFileModes).Int64Slice()
	flags := pkg.Header.GetTag(tagFileFlags).Int64Slice()

	out := make([]repodata.File, 0, len(base))
	for i, name := range base {
		var dir string
		if i < len(ixs) && int(ixs[i]) < len(dirs) {
			dir = dirs[ixs[i]]
		}
		path := dir + name
		ftype := ""
		if i < len(modes) && modes[i]&sIFMT == sIFDIR {
			ftype = "dir"
		}
		if i < len(flags) && flags[i]&rpm.FileFlagGhost != 0 {
			ftype = "ghost"
		}
		out = append(out, repodata.File{Path: path, Type: ftype})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// changelogs reads changelog tags directly (rpm.Package.ChangeLog exposes only
// the text). The rpm header stores changelogs newest-first; createrepo_c emits
// them oldest-first, keeping the most recent limit entries.
func changelogs(pkg *rpm.Package, limit int) []repodata.Changelog {
	times := pkg.Header.GetTag(tagChangelogTime).Int64Slice()
	names := pkg.Header.GetTag(tagChangelogName).StringSlice()
	texts := pkg.Header.GetTag(tagChangelogText).StringSlice()
	n := min(len(names), len(times))
	if len(texts) < n {
		n = len(texts)
	}
	if n == 0 {
		return nil
	}
	// Keep the most recent `limit` entries (the leading entries, newest-first).
	if limit > 0 && n > limit {
		n = limit
	}
	out := make([]repodata.Changelog, 0, n)
	// Reverse to oldest-first.
	for i := n - 1; i >= 0; i-- {
		out = append(out, repodata.Changelog{
			Author: names[i],
			Date:   times[i],
			Text:   texts[i],
		})
	}
	return out
}
