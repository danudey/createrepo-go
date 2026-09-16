// Package repodata defines the data model for rpm-md (dnf/yum) repository
// metadata and (de)serializes the primary, filelists, other and repomd XML
// documents. It targets the metadata consumed by dnf on RHEL 8 and later, so
// it deliberately omits sqlite databases, delta RPMs, comps/groups and
// modularity metadata.
package repodata

// XML namespaces used by rpm-md metadata.
const (
	NSCommon    = "http://linux.duke.edu/metadata/common"
	NSFilelists = "http://linux.duke.edu/metadata/filelists"
	NSOther     = "http://linux.duke.edu/metadata/other"
	NSRepo      = "http://linux.duke.edu/metadata/repo"
	NSRPM       = "http://linux.duke.edu/metadata/rpm"
)

// Package is the union of everything we record about a single RPM across the
// primary, filelists and other metadata documents. A Package is keyed within a
// repository by PkgID (its package checksum).
type Package struct {
	// Identity.
	Name    string
	Arch    string
	Epoch   int
	Version string
	Release string

	// PkgID is the package's checksum, used as <checksum pkgid="YES"> in
	// primary and as the pkgid attribute in filelists/other.
	PkgID        string
	ChecksumType string // e.g. "sha256"

	// Primary descriptive fields.
	Summary     string
	Description string
	Packager    string
	URL         string
	FileTime    int64  // <time file=...>: mtime of the .rpm on disk
	BuildTime   int64  // <time build=...>: RPMTAG_BUILDTIME
	SizePackage int64  // on-disk size of the .rpm
	SizeInstall int64  // RPMTAG_SIZE (sum of installed file sizes)
	SizeArchive int64  // RPMTAG_ARCHIVESIZE (0 when unset)
	Location    string // href relative to the repo root, e.g. "Packages/foo.rpm"

	// XMLBase is the <location xml:base="..."> attribute: an absolute URL that
	// Location is resolved against. When set, clients fetch the package from
	// that URL instead of from the repository they read the metadata from, so a
	// copied repository keeps serving its packages from the original host. It is
	// empty for the self-contained repositories this tool writes.
	XMLBase string

	// <format> fields.
	License     string
	Vendor      string
	Group       string
	BuildHost   string
	SourceRPM   string
	HeaderStart int
	HeaderEnd   int

	Provides  []Entry
	Requires  []Entry
	Conflicts []Entry
	Obsoletes []Entry

	// Files holds every file in the package (used for filelists; the primary
	// document emits only the "primary" subset, see PrimaryFiles).
	Files []File

	// Changelogs are ordered oldest-first, matching createrepo_c output.
	Changelogs []Changelog
}

// Entry is a single dependency relation (a provides/requires/conflicts/
// obsoletes entry).
type Entry struct {
	Name  string
	Flags string // "", "EQ", "LT", "GT", "LE", "GE"
	Epoch int
	Ver   string
	Rel   string
	Pre   bool // requires only: a pre-install/scriptlet dependency
}

// File describes one packaged file path.
type File struct {
	Path string
	Type string // "" (regular file), "dir", or "ghost"
}

// Changelog is a single changelog entry.
type Changelog struct {
	Author string
	Date   int64
	Text   string
}

// NEVRA returns the canonical name-epoch:version-release.arch identifier.
func (p *Package) NEVRA() string {
	return formatNEVRA(p.Name, p.Epoch, p.Version, p.Release, p.Arch)
}

// EVR returns the epoch:version-release portion used for matching.
func (p *Package) EVR() string {
	return formatEVR(p.Epoch, p.Version, p.Release)
}
