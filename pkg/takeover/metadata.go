package takeover

import (
	"path"
	"strings"
)

// The metadata documents this tool generates. Everything else repomd.xml
// references is a document it cannot write, so a republish drops it.
var generated = map[string]bool{"primary": true, "filelists": true, "other": true}

// sqliteMetadata names the sqlite databases as a group: three repomd types
// (primary_db, filelists_db, other_db) that stand or fall together.
const sqliteMetadata = "sqlite metadata"

// metadataImpact describes what losing one kind of metadata costs.
type metadataImpact struct {
	severity Severity
	// what names the metadata in the finding's summary.
	what string
	// consequence completes the sentence "…would be dropped, so …".
	consequence string
	fix         string
}

// impacts maps a repomd data type to what dropping it means for clients. The
// types are matched by prefix so the _db/_gz/_zck variants of each are covered.
var impacts = []struct {
	prefix string
	impact metadataImpact
}{
	{"primary_db", metadataImpact{
		severity: Advisory,
		what:     sqliteMetadata,
		consequence: "nothing on RHEL 8 or later reads it: dnf uses the XML documents and " +
			"has not read the sqlite databases since yum. Older yum clients (RHEL 7 and earlier) do read them",
	}},
	{"filelists_db", metadataImpact{severity: Advisory, what: sqliteMetadata, consequence: "see primary_db"}},
	{"other_db", metadataImpact{severity: Advisory, what: sqliteMetadata, consequence: "see primary_db"}},
	{"group", metadataImpact{
		severity:    Blocking,
		what:        "comps (package group) metadata",
		consequence: "`dnf group list/install` and environment groups stop working for this repository",
		fix:         "keep publishing the comps document with the tooling that generates it, or accept the loss deliberately",
	}},
	{"updateinfo", metadataImpact{
		severity: Blocking,
		what:     "updateinfo (errata/advisory) metadata",
		consequence: "`dnf updateinfo`, `dnf update --security` and advisory-based patching stop working, " +
			"and scanners that read advisories will report this repository as carrying none",
		fix: "keep publishing updateinfo with the tooling that generates it, or accept the loss deliberately",
	}},
	{"modules", metadataImpact{
		severity:    Blocking,
		what:        "modulemd (modularity) metadata",
		consequence: "modular content becomes uninstallable: dnf cannot resolve module streams or profiles",
		fix:         "this tool does not write modulemd; do not take over a modular repository with it",
	}},
	{"prestodelta", metadataImpact{
		severity:    Advisory,
		what:        "delta RPM metadata",
		consequence: "clients fall back to downloading whole packages instead of deltas",
	}},
	{"deltainfo", metadataImpact{
		severity:    Advisory,
		what:        "delta RPM metadata",
		consequence: "clients fall back to downloading whole packages instead of deltas",
	}},
	{"appstream", metadataImpact{
		severity:    Advisory,
		what:        "AppStream metadata",
		consequence: "GNOME Software and similar front ends lose this repository's application catalogue",
	}},
}

// impactOf returns what dropping the named metadata type costs. An unrecognized
// type is advisory: it is something the repository publishes deliberately, and
// this tool cannot say what depends on it.
func impactOf(typ string) metadataImpact {
	for _, e := range impacts {
		if strings.HasPrefix(typ, e.prefix) {
			return e.impact
		}
	}
	return metadataImpact{
		severity:    Advisory,
		what:        "metadata this tool does not generate",
		consequence: "whatever consumes it stops finding it",
	}
}

// compressionOf names the compression of a metadata file from its extension,
// in the vocabulary --compression uses.
func compressionOf(href string) string {
	switch strings.ToLower(path.Ext(href)) {
	case ".gz":
		return "gzip"
	case ".zst", ".zstd":
		return "zstd"
	case ".xz":
		return "xz"
	case ".bz2":
		return "bzip2"
	case ".zck":
		return "zchunk"
	default:
		return "none"
	}
}

// dirOf returns the directory a package href lives in, with "" meaning the
// repository root. It is what --location-prefix records.
func dirOf(href string) string {
	d := path.Dir(href)
	if d == "." || d == "/" {
		return ""
	}
	return strings.Trim(d, "/")
}

// uniqueSorted returns the distinct non-empty values of in, sorted.
func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sortStrings(out)
	return out
}

// truncate limits a list of example items to n entries, appending a count of
// what was left out so a report never hides how much it is summarizing.
func truncate(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	out := append([]string(nil), items[:n]...)
	return append(out, plural(len(items)-n, "more", "more"))
}
