package repo

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/compress"
	"github.com/danudey/createrepo-go/pkg/integrity"
	"github.com/danudey/createrepo-go/pkg/rpm/repodata"
	"github.com/danudey/createrepo-go/pkg/xfer"
)

// The repomd.xml data types of the package documents a Repo loads and writes.
const (
	typePrimary   = "primary"
	typeFilelists = "filelists"
	typeOther     = "other"
)

// maxRepomdSize bounds repomd.xml, which lists a few documents and is a few
// kilobytes in practice.
const maxRepomdSize = 16 << 20

// MetadataFile is one repomd.xml data entry as CheckMetadata found it.
type MetadataFile struct {
	Type     string
	Location string
	// Size is the size read back; Detail says what was checked or why it
	// failed; OK is false when it is one of the report's problems.
	Size   int64
	OK     bool
	Detail string
}

// MetadataReport is what CheckMetadata found.
type MetadataReport struct {
	// Repomd is the parsed repomd.xml, nil when it is missing or unreadable;
	// nothing else can be checked then.
	Repomd *repodata.Repomd
	// Files lists every data entry repomd.xml references, in its order.
	Files    []MetadataFile
	Problems []Problem
	Warnings []Problem
	// Cache holds the raw bytes of the documents a Repo loads (primary,
	// filelists, other) by location, for Options.MetadataCache, so verify
	// does not download them twice.
	Cache map[string][]byte

	// What the package documents hold, for checking them against each
	// other once all three are read.
	primary  []*repodata.Package
	declared map[string]int // the packages="N" each document states
	refs     map[string][]pkgRef
}

// pkgRef is one <package> entry of filelists.xml or other.xml.
type pkgRef struct{ pkgid, name, arch string }

// Usable reports whether the package index can be loaded: repomd.xml and
// primary.xml are both sound.
func (m *MetadataReport) Usable() bool {
	if m.Repomd == nil {
		return false
	}
	for _, f := range m.Files {
		if f.Type == typePrimary {
			return f.OK
		}
	}
	return false
}

// CheckMetadata checks a repository's metadata as a client reads it:
// repomd.xml is present and parses, it names a primary document and no type
// twice, and every document it references is present with the recorded size
// and checksum, decompresses, and has the recorded open size and checksum. The
// package documents (primary, filelists, other) must also parse. Every defect
// is reported rather than stopping the check, so one pass shows them all.
func CheckMetadata(ctx context.Context, be backend.Backend) *MetadataReport {
	rep := &MetadataReport{Cache: map[string][]byte{}, declared: map[string]int{}, refs: map[string][]pkgRef{}}
	problem := func(subject, loc, detail string) {
		rep.Problems = append(rep.Problems, Problem{Kind: KindMetadata, Subject: subject, Location: loc, Detail: detail})
	}

	raw, err := xfer.ReadAllLimit(ctx, be, repomdPath, maxRepomdSize)
	switch {
	case errors.Is(err, backend.ErrNotExist):
		problem(repomdPath, "", "there is no repodata/repomd.xml: this is not a repository, or not its root")
		return rep
	case err != nil:
		problem(repomdPath, "", "it cannot be read: "+err.Error())
		return rep
	}
	md, err := repodata.ParseRepomd(raw)
	if err != nil {
		problem(repomdPath, "", "it does not parse: "+err.Error())
		return rep
	}
	rep.Repomd = md

	seen := map[string]bool{}
	for _, d := range md.Data {
		if seen[d.Type] {
			problem(repomdPath, "", fmt.Sprintf("it lists the %s document more than once; clients use one and ignore the rest", d.Type))
		}
		seen[d.Type] = true
	}
	if !seen[typePrimary] {
		problem(repomdPath, "", "it lists no primary document, so a client sees no packages")
	}
	for _, typ := range []string{typeFilelists, typeOther} {
		if !seen[typ] {
			rep.Warnings = append(rep.Warnings, Problem{
				Kind: KindMetadata, Subject: repomdPath,
				Detail: fmt.Sprintf("it lists no %s document; dnf resolves dependencies on files outside /etc and /usr/bin with filelists, and shows changelogs from other", typ),
			})
		}
	}

	for _, d := range md.Data {
		f := checkDataEntry(ctx, be, d, rep)
		rep.Files = append(rep.Files, f)
		if !f.OK {
			problem(d.Type+" metadata", d.Location, f.Detail)
		}
		if weakChecksum(d.ChecksumType) {
			rep.Warnings = append(rep.Warnings, Problem{
				Kind: KindMetadata, Subject: d.Type + " metadata", Location: d.Location,
				Detail: fmt.Sprintf("repomd.xml records a %s checksum for it; %s", d.ChecksumType, weakWhy),
			})
		}
	}
	crossCheck(rep)
	return rep
}

// weakWhy says why a weak checksum type matters.
const weakWhy = "it is a weak hash, which a system in FIPS mode refuses and which does not protect against a deliberately altered file"

// weakChecksum reports a checksum type that is known but weak.
func weakChecksum(algo string) bool {
	switch strings.ToLower(algo) {
	case "md5", "sha", "sha1":
		return true
	}
	return false
}

// crossCheck checks the package documents against each other and primary.xml
// against itself: what dnf would see that the files alone do not show.
func crossCheck(rep *MetadataReport) {
	if rep.primary == nil {
		return
	}
	add := func(warn bool, subject, loc, format string, a ...any) {
		p := Problem{Kind: KindMetadata, Subject: subject, Location: loc, Detail: fmt.Sprintf(format, a...)}
		if warn {
			rep.Warnings = append(rep.Warnings, p)
		} else {
			rep.Problems = append(rep.Problems, p)
		}
	}

	byID := map[string]*repodata.Package{}
	byLoc := map[string]*repodata.Package{}
	byNEVRA := map[string]*repodata.Package{}
	bases := map[string]bool{}
	weak := 0
	for _, p := range rep.primary {
		subject := p.NEVRA()
		switch {
		case p.PkgID == "":
			add(false, subject, p.Location, "primary.xml records no checksum for it, so dnf cannot check the download")
		case byID[p.PkgID] != nil:
			add(false, subject, p.Location, "primary.xml lists its checksum %s again, for %s; dnf keeps one entry and ignores the other", p.PkgID, byID[p.PkgID].NEVRA())
		default:
			byID[p.PkgID] = p
		}
		if _, known := integrity.NewHash(p.ChecksumType); !known && p.PkgID != "" {
			add(false, subject, p.Location, "primary.xml records a checksum of unknown type %q, which dnf cannot check", p.ChecksumType)
		} else if weakChecksum(p.ChecksumType) {
			weak++
		}
		switch err := backend.CheckPath(p.Location); {
		case p.Location == "":
			add(false, subject, "", "primary.xml gives it no location, so dnf cannot download it")
		case err != nil:
			add(false, subject, p.Location, "its location is unsafe: %v", err)
		case byLoc[p.Location] != nil:
			add(false, subject, p.Location, "%s is published at the same location; only one file can be there", byLoc[p.Location].NEVRA())
		default:
			byLoc[p.Location] = p
		}
		if q := byNEVRA[subject]; q != nil && q.PkgID != p.PkgID {
			add(false, subject, p.Location, "primary.xml lists two different packages as %s (also at %s); dnf picks either, and a client that has one installed cannot verify the other", subject, q.Location)
		} else if q == nil {
			byNEVRA[subject] = p
		}
		if p.XMLBase != "" {
			bases[p.XMLBase] = true
		}
	}
	if weak > 0 {
		add(true, "primary metadata", "", "%d package(s) are recorded with a weak checksum (md5 or sha1); %s", weak, weakWhy)
	}
	for b := range bases {
		add(true, "primary metadata", "", "packages carry xml:base=%q, so dnf downloads them from there, not from this repository; verify checked the copies here, which may not be the ones clients get", b)
	}

	if n, ok := rep.declared[typePrimary]; ok && n != len(rep.primary) {
		add(true, "primary metadata", "", "primary.xml says it lists %d package(s) but lists %d", n, len(rep.primary))
	}
	for _, typ := range []string{typeFilelists, typeOther} {
		refs, ok := rep.refs[typ]
		if !ok {
			continue // absent or unreadable, reported already
		}
		if n, ok := rep.declared[typ]; ok && n != len(refs) {
			add(true, typ+" metadata", "", "%s.xml says it lists %d package(s) but lists %d", typ, n, len(refs))
		}
		seen := map[string]bool{}
		var extra int
		for _, r := range refs {
			p := byID[r.pkgid]
			if p == nil {
				extra++
				continue
			}
			if seen[r.pkgid] {
				add(true, p.NEVRA(), p.Location, "%s.xml lists it more than once", typ)
			}
			seen[r.pkgid] = true
			if (r.name != "" && r.name != p.Name) || (r.arch != "" && r.arch != p.Arch) {
				add(false, p.NEVRA(), p.Location, "%s.xml gives its checksum to %s.%s; the documents were not generated together", typ, r.name, r.arch)
			}
		}
		if extra > 0 {
			add(true, typ+" metadata", "", "%s.xml lists %d package(s) primary.xml does not; the documents were not generated together", typ, extra)
		}
		missing := 0
		for id := range byID {
			if !seen[id] {
				missing++
			}
		}
		if missing > 0 {
			// filelists is how dnf resolves a dependency on a file; other
			// only holds changelogs.
			why := "dnf cannot resolve a dependency on a file they hold"
			if typ == typeOther {
				why = "dnf shows no changelog for them"
			}
			add(typ == typeOther, typ+" metadata", "", "%d package(s) in primary.xml are missing from %s.xml, so %s; the documents were not generated together", missing, typ, why)
		}
	}
}

// checkDataEntry checks one document repomd.xml references.
func checkDataEntry(ctx context.Context, be backend.Backend, d repodata.DataEntry, rep *MetadataReport) MetadataFile {
	f := MetadataFile{Type: d.Type, Location: d.Location}
	fail := func(format string, a ...any) MetadataFile {
		f.Detail = fmt.Sprintf(format, a...)
		return f
	}
	if d.Location == "" {
		return fail("repomd.xml gives it no location")
	}
	if err := backend.CheckPath(d.Location); err != nil {
		return fail("its location is unsafe: %v", err)
	}
	// Read no more than repomd.xml records: one byte past it is enough to
	// tell the file is the wrong size.
	limit := int64(maxMetadataSize)
	if d.Size > 0 && d.Size < limit {
		limit = d.Size + 1
	}
	raw, err := xfer.ReadAllLimit(ctx, be, d.Location, limit)
	if errors.Is(err, backend.ErrNotExist) {
		return fail("the file repomd.xml references is missing")
	}
	if errors.Is(err, xfer.ErrTooLarge) {
		return fail("it is larger than the %d bytes repomd.xml records", d.Size)
	}
	if err != nil {
		return fail("it cannot be read: %v", err)
	}
	f.Size = int64(len(raw))
	if d.Size > 0 && f.Size != d.Size {
		return fail("size %d does not match the %d repomd.xml records", f.Size, d.Size)
	}
	if d.Checksum == "" {
		return fail("repomd.xml records no checksum for it, so a client cannot check it")
	}
	if got, ok, known := digest(d.ChecksumType, raw, d.Checksum); !known {
		return fail("repomd.xml records a checksum of unknown type %q", d.ChecksumType)
	} else if !ok {
		return fail("%s checksum %s does not match the %s repomd.xml records", d.ChecksumType, got, d.Checksum)
	}

	// zchunk documents are an alternative encoding of another document that
	// is checked in its own right; this build cannot decode them.
	if strings.HasSuffix(d.Type, "_zck") {
		f.OK, f.Detail = true, fmt.Sprintf("size %d, %s ok; zchunk content not decoded", f.Size, d.ChecksumType)
		return f
	}
	limit = int64(maxMetadataSize)
	if d.OpenSize > 0 && d.OpenSize < limit {
		limit = d.OpenSize
	}
	dr, err := compress.DecompressStream(d.Location, bytes.NewReader(raw))
	if err != nil {
		return fail("it does not decompress: %v", err)
	}
	// One byte past the limit tells an over-long document from one that fits.
	open, err := io.ReadAll(io.LimitReader(dr, limit+1))
	dr.Close()
	if err != nil {
		return fail("it does not decompress: %v", err)
	}
	if d.OpenSize > 0 && int64(len(open)) != d.OpenSize {
		return fail("decompressed size %d does not match the open-size %d repomd.xml records", len(open), d.OpenSize)
	}
	if d.OpenChecksum != "" {
		if got, ok, _ := digest(d.ChecksumType, open, d.OpenChecksum); !ok {
			return fail("decompressed %s checksum %s does not match the open-checksum %s repomd.xml records", d.ChecksumType, got, d.OpenChecksum)
		}
	}

	var parseErr error
	switch d.Type {
	case typePrimary:
		var pkgs []*repodata.Package
		if pkgs, parseErr = repodata.ParsePrimary(open); parseErr == nil {
			rep.primary = pkgs
			if n, ok := declaredCount(open); ok {
				rep.declared[d.Type] = n
			}
		}
	case typeFilelists, typeOther:
		var refs []pkgRef
		var n int
		var ok bool
		if refs, n, ok, parseErr = scanPackages(open); parseErr == nil {
			rep.refs[d.Type] = refs
			if ok {
				rep.declared[d.Type] = n
			}
		}
	}
	if parseErr != nil {
		return fail("it does not parse: %v", parseErr)
	}
	switch d.Type {
	case typePrimary, typeFilelists, typeOther:
		rep.Cache[d.Location] = raw
	}
	f.OK, f.Detail = true, fmt.Sprintf("size %d, %s and open checksum ok", f.Size, d.ChecksumType)
	return f
}

// declaredCount reads the packages="N" attribute of a document's root element.
func declaredCount(doc []byte) (int, bool) {
	dec := xml.NewDecoder(bytes.NewReader(doc))
	for {
		tok, err := dec.Token()
		if err != nil {
			return 0, false
		}
		if se, ok := tok.(xml.StartElement); ok {
			return packagesAttr(se)
		}
	}
}

func packagesAttr(se xml.StartElement) (int, bool) {
	for _, a := range se.Attr {
		if a.Name.Local == "packages" {
			n, err := strconv.Atoi(a.Value)
			return n, err == nil
		}
	}
	return 0, false
}

// scanPackages reads filelists.xml or other.xml for its root's packages="N"
// and each <package>'s pkgid, name and arch, checking the document is
// well-formed XML on the way.
func scanPackages(doc []byte) (refs []pkgRef, declared int, hasDeclared bool, err error) {
	dec := xml.NewDecoder(bytes.NewReader(doc))
	depth, sawRoot := 0, false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			if sawRoot && depth == 0 {
				return refs, declared, hasDeclared, nil
			}
			return nil, 0, false, io.ErrUnexpectedEOF
		}
		if err != nil {
			return nil, 0, false, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case depth == 1:
				sawRoot = true
				declared, hasDeclared = packagesAttr(t)
			case depth == 2 && t.Name.Local == "package":
				var r pkgRef
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "pkgid":
						r.pkgid = a.Value
					case "name":
						r.name = a.Value
					case "arch":
						r.arch = a.Value
					}
				}
				refs = append(refs, r)
			}
		case xml.EndElement:
			depth--
		}
	}
}

// digest hashes data with the named checksum type and compares it with want.
func digest(algo string, data []byte, want string) (got string, ok, known bool) {
	h, known := integrity.NewHash(algo)
	if !known {
		return "", false, false
	}
	h.Write(data)
	got = hex.EncodeToString(h.Sum(nil))
	return got, strings.EqualFold(got, want), true
}
