/*
Package rpm implements the rpm package file format.

This is a copy of github.com/cavaliergopher/rpm v1.3.0, BSD-3-Clause, see
LICENSE in this directory. It is vendored rather than imported because the
upstream package contains signature.go, which imports golang.org/x/crypto/
openpgp. That package is unmaintained and carries GO-2026-5932, an advisory
with no fix and no prospect of one; importing upstream at all made the
advisory reachable from this module's package initialization, whether or not
any signature function was ever called. Upstream has published nothing since
v1.3.0, so waiting was not an option either.

What changed from upstream:

  - signature.go is gone, with it GPGCheck, MD5Check, ReadKeyRing,
    OpenKeyRing and the GPGSignature type. Nothing here used them. Signature
    verification in this project lives in pkg/sign, which shells out to
    rpmkeys and gpg and, where it parses OpenPGP itself, uses the maintained
    github.com/ProtonMail/go-crypto.
  - Package.GPGSignature returns the raw tag bytes instead of the removed
    GPGSignature type.
  - cmd/rpmdump and cmd/rpminfo are not copied.
  - gofumpt rewrote one octal literal, 0777 to 0o777, because the repository
    gates on gofumpt over the whole tree.

Everything else is upstream, unmodified, so that a future upstream release
can be diffed against it. .golangci.yml excludes this directory for the same
reason; the linters have plenty to say about it and none of it is actionable
without losing that property. The header parsing is exercised by the golden
tests in pkg/rpmmeta, which assert byte offsets, dependencies, file lists and
changelogs against createrepo_c's own output for the reference fixtures.

For more information about the rpm file format, see:

http://ftp.rpm.org/max-rpm/s1-rpm-file-format-rpm-file-format.html

Packages are composed of two headers: the Signature header and the "Header"
header. Each contains key-value pairs called tags. Tags map an integer key to a
value whose data type will be one of the TagType types. Tag values can be
decoded with the appropriate Tag method for the data type.

Many known tags are available as Package methods. For example, RPMTAG_NAME and
RPMTAG_BUILDTIME are available as Package.Name and Package.BuildTime
respectively.

	fmt.Println(pkg.Name(), pkg.BuildTime())

Tags can be retrieved and decoded from the Signature or Header headers directly
using Header.GetTag and their tag identifier.

	const (
		RPMTagName      = 1000
		RPMTagBuidlTime = 1006
	)

	fmt.Println(
		pkg.Header.GetTag(RPMTagName).String()),
		time.Unix(pkg.Header.GetTag(RPMTagBuildTime).Int64(), 0),
	)

Header.GetTag and all Tag methods will return a zero value if the header or the
tag do not exist, or if the tag has a different data type.

You may enumerate all tags in a header with Header.Tags:

	for id, tag := range pkg.Header.Tags {
		fmt.Println(id, tag.Type, tag.Value)
	}

# Comparing versions

In the rpm ecosystem, package versions are compared using EVR; epoch, version,
release. Versions may be compared using the Compare function.

	if rpm.Compare(pkgA, pkgB) == 1 {
		fmt.Println("A is more recent than B")
	}

Packages may be be sorted using the PackageSlice type which implements
sort.Interface. Packages are sorted lexically by name ascending and then by
version descending. Version is evaluated first by epoch, then by version string,
then by release.

	sort.Sort(PackageSlice(pkgs))

The Sort function is provided for your convenience.

	rpm.Sort(pkgs)

# Extracting files

The payload of an rpm package is typically archived in cpio format and
compressed with xz. To decompress and unarchive an rpm payload, the reader that
read the rpm package headers will be positioned at the beginning of the payload
and can be reused with the appropriate Go packages for the rpm payload format.

You can check the archive format with Package.PayloadFormat and the compression
algorithm with Package.PayloadCompression.
*/
package rpm
