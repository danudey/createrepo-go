package takeover

import (
	"fmt"
	"sort"
	"strings"

	"github.com/danudey/createrepo-go/pkg/repodata"
)

// diffPackages compares the metadata that would be written for each package
// against the metadata the repository publishes for it today, and reports the
// fields that differ along with a few example packages.
//
// The published side is read from the primary, filelists and other documents as
// they stand; the current side is the in-memory index a republish would render.
// With the packages taken at their word the two sides differ only where this
// tool's rendering differs from the original tool's (the primary document's file
// subset is the usual one). After --from-packages they differ wherever the
// published metadata was wrong about the file, which is exactly what the
// operator needs to see before republishing it.
//
// Packages are matched by NEVRA, because --from-packages changes the pkgid of a
// package whose file no longer matches its metadata, and relocation changes its
// location; its name, version and architecture are what stay put.
func diffPackages(published []*repodata.Package, filelists map[string][]repodata.File,
	changelogs map[string][]repodata.Changelog, current []*repodata.Package,
) []FieldDiff {
	byNEVRA := make(map[string]*repodata.Package, len(current))
	for _, p := range current {
		byNEVRA[p.NEVRA()] = p
	}

	counts := map[string][]string{}
	note := func(field string, p *repodata.Package) {
		counts[field] = append(counts[field], p.NEVRA())
	}

	for _, old := range published {
		now, ok := byNEVRA[old.NEVRA()]
		if !ok {
			note("package removed from the index", old)
			continue
		}
		diffScalars(old, now, note)
		diffDeps(old, now, note)
		if !sameFiles(old.Files, now.PrimaryFiles()) {
			note("primary file list", now)
		}
		if filelists != nil && !sameFiles(filelists[old.PkgID], now.Files) {
			note("filelists", now)
		}
		if changelogs != nil && !sameChangelogs(changelogs[old.PkgID], now.Changelogs) {
			note("changelogs", now)
		}
	}

	fields := make([]string, 0, len(counts))
	for f := range counts {
		fields = append(fields, f)
	}
	sort.Strings(fields)

	out := make([]FieldDiff, 0, len(fields))
	for _, f := range fields {
		items := counts[f]
		out = append(out, FieldDiff{Field: f, Packages: len(items), Examples: truncate(items, 3)})
	}
	return out
}

// diffScalars compares the single-valued fields of a package's metadata.
func diffScalars(old, now *repodata.Package, note func(string, *repodata.Package)) {
	for _, c := range []struct {
		field    string
		old, now any
	}{
		{"checksum (pkgid)", old.PkgID, now.PkgID},
		{"checksum type", strings.ToLower(old.ChecksumType), strings.ToLower(now.ChecksumType)},
		{"location", old.Location, now.Location},
		{"xml:base", old.XMLBase, now.XMLBase},
		{"package size", old.SizePackage, now.SizePackage},
		{"installed size", old.SizeInstall, now.SizeInstall},
		{"archive size", old.SizeArchive, now.SizeArchive},
		{"file time", old.FileTime, now.FileTime},
		{"build time", old.BuildTime, now.BuildTime},
		{"summary", old.Summary, now.Summary},
		{"description", old.Description, now.Description},
		{"packager", old.Packager, now.Packager},
		{"url", old.URL, now.URL},
		{"license", old.License, now.License},
		{"vendor", old.Vendor, now.Vendor},
		{"group", old.Group, now.Group},
		{"build host", old.BuildHost, now.BuildHost},
		{"source rpm", old.SourceRPM, now.SourceRPM},
		{"header range", [2]int{old.HeaderStart, old.HeaderEnd}, [2]int{now.HeaderStart, now.HeaderEnd}},
	} {
		if c.old != c.now {
			note(c.field, now)
		}
	}
}

// diffDeps compares the four dependency lists.
func diffDeps(old, now *repodata.Package, note func(string, *repodata.Package)) {
	for _, c := range []struct {
		field    string
		old, now []repodata.Entry
	}{
		{"provides", old.Provides, now.Provides},
		{"requires", old.Requires, now.Requires},
		{"conflicts", old.Conflicts, now.Conflicts},
		{"obsoletes", old.Obsoletes, now.Obsoletes},
	} {
		if !sameEntries(c.old, c.now) {
			note(c.field, now)
		}
	}
}

// sameEntries compares two dependency lists as sets: rpm-md does not define an
// order for them, and reporting a reordering as a change would bury the real
// differences.
func sameEntries(a, b []repodata.Entry) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(e repodata.Entry) string {
		return fmt.Sprintf("%s|%s|%d|%s|%s|%t", e.Name, e.Flags, e.Epoch, e.Ver, e.Rel, e.Pre)
	}
	return sameKeyed(a, b, key)
}

// sameFiles compares two file lists as sets.
func sameFiles(a, b []repodata.File) bool {
	if len(a) != len(b) {
		return false
	}
	return sameKeyed(a, b, func(f repodata.File) string { return f.Type + "|" + f.Path })
}

// sameChangelogs compares two changelog lists as sets.
func sameChangelogs(a, b []repodata.Changelog) bool {
	if len(a) != len(b) {
		return false
	}
	return sameKeyed(a, b, func(c repodata.Changelog) string {
		return fmt.Sprintf("%d|%s|%s", c.Date, c.Author, c.Text)
	})
}

// sameKeyed reports whether two slices hold the same multiset of items, as
// judged by key.
func sameKeyed[T any](a, b []T, key func(T) string) bool {
	seen := make(map[string]int, len(a))
	for _, it := range a {
		seen[key(it)]++
	}
	for _, it := range b {
		k := key(it)
		if seen[k] == 0 {
			return false
		}
		seen[k]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
