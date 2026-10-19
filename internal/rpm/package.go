package rpm

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"syscall"
	"time"
)

// A Package is an rpm package file.
type Package struct {
	Lead      Lead
	Signature Header
	Header    Header
}

var _ Version = &Package{}

// Read reads an rpm package from r.
//
// When this function returns, the reader will be positioned at the start of the
// package payload. Use Package.PayloadFormat and Package.PayloadCompression to
// determine how to decompress and unarchive the payload.
func Read(r io.Reader) (*Package, error) {
	lead, err := readLead(r)
	if err != nil {
		return nil, err
	}
	sig, err := readHeader(r, true)
	if err != nil {
		return nil, err
	}
	hdr, err := readHeader(r, false)
	if err != nil {
		return nil, err
	}
	return &Package{
		Lead:      *lead,
		Signature: *sig,
		Header:    *hdr,
	}, nil
}

// Open opens an rpm package from the file system.
//
// Once the package headers are read, the underlying reader is closed and cannot
// be used to read the package payload. To read the package payload, open the
// package with os.Open and read the headers with Read. You may then use the
// same reader to read the payload.
func Open(name string) (*Package, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Read(bufio.NewReader(f))
}

// dependencies translates the given tag values into a slice of package
// relationships such as provides, conflicts, obsoletes and requires.
func (c *Package) dependencies(nevrsTagID, flagsTagID, namesTagID, versionsTagID int) []Dependency {
	// TODO: Implement NEVRS tags
	flgs := c.Header.GetTag(flagsTagID).Int64Slice()
	names := c.Header.GetTag(namesTagID).StringSlice()
	vers := c.Header.GetTag(versionsTagID).StringSlice()
	deps := make([]Dependency, len(names))
	for i := range names {
		// The three arrays are parallel in any package rpm would accept, but
		// the header is untrusted input: a short array reads as zero values
		// here rather than a panic. Validate reports the inconsistency.
		var flags int
		if i < len(flgs) {
			flags = int(flgs[i])
		}
		var epoch int
		var ver, rel string
		if i < len(vers) {
			epoch, ver, rel = parseVersion(vers[i])
		}
		deps[i] = &dependency{
			flags:   flags,
			name:    names[i],
			epoch:   epoch,
			version: ver,
			release: rel,
		}
	}
	return deps
}

// parallelArrays lists header tags whose values are indexed together: every
// tag in a group present in a header must have the same number of values as
// the group's first tag.
var parallelArrays = []struct {
	what string
	tags []int
}{
	{"requires", []int{1049, 1048, 1050}},
	{"provides", []int{1047, 1112, 1113}},
	{"conflicts", []int{1054, 1053, 1055}},
	{"obsoletes", []int{1090, 1114, 1115}},
	{"recommends", []int{5046, 5048, 5047}},
	{"suggests", []int{5049, 5051, 5050}},
	{"supplements", []int{5052, 5054, 5053}},
	{"enhances", []int{5055, 5057, 5056}},
	{"file list", []int{1117, 1116, 1030, 1037, 1028, 5008, 1034, 1035, 1036, 1039, 1040}},
	{"changelog", []int{1080, 1081, 1082}},
}

// Validate reports structural inconsistencies in the package header that
// parsing alone does not catch: a missing name, version or release, parallel
// arrays of different lengths, and file entries whose directory index points
// outside the directory list. Accessors tolerate all of these, so a caller
// that publishes what it reads should call Validate first.
func (c *Package) Validate() error {
	for _, req := range []struct {
		id   int
		name string
	}{{1000, "name"}, {1001, "version"}, {1002, "release"}} {
		if c.Header.GetTag(req.id).String() == "" {
			return errorf("the header has no %s", req.name)
		}
	}
	for _, g := range parallelArrays {
		want := tagLen(c.Header.GetTag(g.tags[0]))
		for _, id := range g.tags[1:] {
			t := c.Header.GetTag(id)
			if t == nil {
				continue
			}
			if n := tagLen(t); n != want {
				return errorf("%s arrays disagree: tag %d has %d values, tag %d has %d", g.what, g.tags[0], want, id, n)
			}
		}
	}
	dirs := len(c.Header.GetTag(1118).StringSlice())
	for i, ix := range c.Header.GetTag(1116).Int64Slice() {
		if ix < 0 || ix >= int64(dirs) {
			return errorf("file %d names directory %d, but there are only %d", i, ix, dirs)
		}
	}
	return nil
}

// tagLen is the number of values a tag holds, whatever its type.
func tagLen(t *Tag) int {
	if t == nil {
		return 0
	}
	switch v := t.Value.(type) {
	case []string:
		return len(v)
	case []int64:
		return len(v)
	case []byte:
		return len(v)
	}
	return 0
}

// String returns the package identifier in the form
// '[name]-[version]-[release].[architecture]'.
func (c *Package) String() string {
	return fmt.Sprintf("%s-%s-%s.%s", c.Name(), c.Version(), c.Release(), c.Architecture())
}

// GPGSignature returns the raw RPMSIGTAG_PGP value. Upstream wraps it in a
// GPGSignature type whose String method decodes the packet with
// golang.org/x/crypto/openpgp; that dependency is why this copy exists, so
// the bytes are returned undecorated. See the package comment.
func (c *Package) GPGSignature() []byte {
	return c.Signature.GetTag(1002).Bytes()
}

// For tag definitions, see:
// https://github.com/rpm-software-management/rpm/blob/master/include/rpm/rpmtag.h#L34

func (c *Package) Name() string {
	return c.Header.GetTag(1000).String()
}

func (c *Package) Version() string {
	return c.Header.GetTag(1001).String()
}

func (c *Package) Release() string {
	return c.Header.GetTag(1002).String()
}

func (c *Package) Epoch() int {
	return int(c.Header.GetTag(1003).Int64())
}

// HeaderRange returns the byte offsets of the RPM header.
func (c *Package) HeaderRange() (start, end int) {
	start = 96 + c.Signature.Size
	end = start + c.Header.Size
	return start, end
}

func (c *Package) Requires() []Dependency {
	return c.dependencies(5041, 1048, 1049, 1050)
}

func (c *Package) Provides() []Dependency {
	return c.dependencies(5042, 1112, 1047, 1113)
}

func (c *Package) Conflicts() []Dependency {
	return c.dependencies(5044, 1053, 1054, 1055)
}

func (c *Package) Obsoletes() []Dependency {
	return c.dependencies(5043, 1114, 1090, 1115)
}

func (c *Package) Suggests() []Dependency {
	return c.dependencies(5059, 5051, 5049, 5050)
}

func (c *Package) Enhances() []Dependency {
	return c.dependencies(5061, 5057, 5055, 5056)
}

func (c *Package) Recommends() []Dependency {
	return c.dependencies(5058, 5048, 5046, 5047)
}

func (c *Package) Supplements() []Dependency {
	return c.dependencies(5060, 5051, 5052, 5053)
}

// Files returns file information for each file that is installed by this RPM
// package.
func (c *Package) Files() []FileInfo {
	ixs := c.Header.GetTag(1116).Int64Slice()
	names := c.Header.GetTag(1117).StringSlice()
	dirs := c.Header.GetTag(1118).StringSlice()
	modes := c.Header.GetTag(1030).Int64Slice()
	sizes := c.Header.GetTag(1028).Int64Slice()
	times := c.Header.GetTag(1034).Int64Slice()
	flags := c.Header.GetTag(1037).Int64Slice()
	owners := c.Header.GetTag(1039).StringSlice()
	groups := c.Header.GetTag(1040).StringSlice()
	digests := c.Header.GetTag(1035).StringSlice()
	linknames := c.Header.GetTag(1036).StringSlice()
	a := make([]FileInfo, len(names))
	for i := range names {
		a[i] = FileInfo{
			name:     dirs[ixs[i]] + names[i],
			mode:     fileModeFromInt64(modes[i]),
			size:     sizes[i],
			modTime:  time.Unix(times[i], 0),
			flags:    flags[i],
			owner:    owners[i],
			group:    groups[i],
			digest:   digests[i],
			linkname: linknames[i],
		}
	}
	return a
}

// fileModeFromInt64 converts the 16 bit value returned from a typical
// unix/linux stat call to the bitmask that go uses to produce an os
// neutral representation.  It is incorrect to just cast the 16 bit
// value directly to a os.FileMode.  The result of stat is 4 bits to
// specify the type of the object, this is a value in the range 0 to
// 15, rather than a bitfield, 3 bits to note suid, sgid and sticky,
// and 3 sets of 3 bits for rwx permissions for user, group and other.
// An os.FileMode has the same 9 bits for permissions, but rather than
// using an enum for the type it has individual bits.  As a concrete
// example, a block device has the 1<<26 bit set (os.ModeDevice) in
// the os.FileMode, but has type 0x6000 (syscall.S_IFBLK). A regular
// file is represented in os.FileMode by not having any of the bits in
// os.ModeType set (i.e. is not a directory, is not a symlink, is not
// a named pipe...) whilst a regular file has value syscall.S_IFREG
// (0x8000) in the mode field from stat.
func fileModeFromInt64(mode int64) os.FileMode {
	fm := os.FileMode(mode & 0o777)
	switch mode & syscall.S_IFMT {
	case syscall.S_IFBLK:
		fm |= os.ModeDevice
	case syscall.S_IFCHR:
		fm |= os.ModeDevice | os.ModeCharDevice
	case syscall.S_IFDIR:
		fm |= os.ModeDir
	case syscall.S_IFIFO:
		fm |= os.ModeNamedPipe
	case syscall.S_IFLNK:
		fm |= os.ModeSymlink
	case syscall.S_IFREG:
		// nothing to do
	case syscall.S_IFSOCK:
		fm |= os.ModeSocket
	}
	if mode&syscall.S_ISGID != 0 {
		fm |= os.ModeSetgid
	}
	if mode&syscall.S_ISUID != 0 {
		fm |= os.ModeSetuid
	}
	if mode&syscall.S_ISVTX != 0 {
		fm |= os.ModeSticky
	}
	return fm
}

// Summary returns the package summary in the C locale, as rpm and createrepo_c
// report it.
func (c *Package) Summary() string { return c.i18nString(1004) }

// Description returns the package description in the C locale.
func (c *Package) Description() string { return c.i18nString(1005) }

// i18nString picks the C-locale value of an I18NSTRING tag. Such a tag holds
// one value per locale, in the order of the header's locale table
// (HEADERI18NTABLE, tag 100), which rpmbuild starts with "C". Joining the
// values, as this used to, put every translation into the summary.
func (c *Package) i18nString(tag int) string {
	values := c.Header.GetTag(tag).StringSlice()
	if len(values) == 0 {
		return ""
	}
	for i, locale := range c.Header.GetTag(100).StringSlice() {
		if locale == "C" && i < len(values) {
			return values[i]
		}
	}
	return values[0]
}

func (c *Package) BuildTime() time.Time {
	return time.Unix(c.Header.GetTag(1006).Int64(), 0)
}

func (c *Package) BuildHost() string {
	return c.Header.GetTag(1007).String()
}

func (c *Package) InstallTime() time.Time {
	return time.Unix(c.Header.GetTag(1008).Int64(), 0)
}

// Size specifies the disk space consumed by installation of the package.
func (c *Package) Size() uint64 {
	if i := uint64(c.Header.GetTag(5009).Int64()); i > 0 {
		return i
	}
	return uint64(c.Header.GetTag(1009).Int64())
}

// ArchiveSize specifies the size of the archived payload of the package in
// bytes.
func (c *Package) ArchiveSize() uint64 {
	if i := uint64(c.Signature.GetTag(271).Int64()); i > 0 {
		return i
	}
	if i := uint64(c.Signature.GetTag(1007).Int64()); i > 0 {
		return i
	}
	if i := uint64(c.Header.GetTag(271).Int64()); i > 0 {
		return i
	}
	return uint64(c.Header.GetTag(1046).Int64())
}

func (c *Package) Distribution() string {
	return c.Header.GetTag(1010).String()
}

func (c *Package) Vendor() string {
	return c.Header.GetTag(1011).String()
}

func (c *Package) GIFImage() []byte {
	return c.Header.GetTag(1012).Bytes()
}

func (c *Package) XPMImage() []byte {
	return c.Header.GetTag(1013).Bytes()
}

func (c *Package) License() string {
	return c.Header.GetTag(1014).String()
}

func (c *Package) Packager() string {
	return c.Header.GetTag(1015).String()
}

func (c *Package) Groups() []string {
	return c.Header.GetTag(1016).StringSlice()
}

func (c *Package) ChangeLog() []string {
	return c.Header.GetTag(1017).StringSlice()
}

func (c *Package) Source() []string {
	return c.Header.GetTag(1018).StringSlice()
}

func (c *Package) Patch() []string {
	return c.Header.GetTag(1019).StringSlice()
}

func (c *Package) URL() string {
	return c.Header.GetTag(1020).String()
}

func (c *Package) OperatingSystem() string {
	return c.Header.GetTag(1021).String()
}

func (c *Package) Architecture() string {
	return c.Header.GetTag(1022).String()
}

func (c *Package) PreInstallScript() string {
	return c.Header.GetTag(1023).String()
}

func (c *Package) PostInstallScript() string {
	return c.Header.GetTag(1024).String()
}

func (c *Package) PreUninstallScript() string {
	return c.Header.GetTag(1025).String()
}

func (c *Package) PostUninstallScript() string {
	return c.Header.GetTag(1026).String()
}

func (c *Package) OldFilenames() []string {
	return c.Header.GetTag(1027).StringSlice()
}

func (c *Package) Icon() []byte {
	return c.Header.GetTag(1043).Bytes()
}

func (c *Package) SourceRPM() string {
	return c.Header.GetTag(1044).String()
}

func (c *Package) RPMVersion() string {
	return c.Header.GetTag(1064).String()
}

func (c *Package) Platform() string {
	return c.Header.GetTag(1132).String()
}

// PayloadFormat returns the name of the format used for the package payload.
// Typically cpio.
func (c *Package) PayloadFormat() string {
	return c.Header.GetTag(1124).String()
}

// PayloadCompression returns the name of the compression used for the package
// payload. Typically xz.
func (c *Package) PayloadCompression() string {
	return c.Header.GetTag(1125).String()
}

// Sort sorts a slice of packages lexically by name ascending and then by
// version descending. Version is evaluated first by epoch, then by version
// string, then by release.
func Sort(x []*Package) { sort.Sort(PackageSlice(x)) }

// PackageSlice implements sort.Interface for a slice of packages. Packages are
// sorted lexically by name ascending and then by version descending. Version is
// evaluated first by epoch, then by version string, then by release.
type PackageSlice []*Package

// Sort is a convenience method: x.Sort() calls sort.Sort(x).
func (x PackageSlice) Sort() { sort.Sort(x) }

func (x PackageSlice) Len() int { return len(x) }

func (x PackageSlice) Less(i, j int) bool {
	a, b := x[i].Name(), x[j].Name()
	if a == b {
		return Compare(x[i], x[j]) == 1
	}
	return a < b
}

func (x PackageSlice) Swap(i, j int) { x[i], x[j] = x[j], x[i] }

var _ sort.Interface = PackageSlice{}
