package repo

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
}

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
	rep := &MetadataReport{Cache: map[string][]byte{}}
	problem := func(subject, loc, detail string) {
		rep.Problems = append(rep.Problems, Problem{Kind: KindMetadata, Subject: subject, Location: loc, Detail: detail})
	}

	raw, err := xfer.ReadAll(ctx, be, repomdPath)
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
	}
	return rep
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
	raw, err := xfer.ReadAll(ctx, be, d.Location)
	if errors.Is(err, backend.ErrNotExist) {
		return fail("the file repomd.xml references is missing")
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
	limit := int64(maxMetadataSize)
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
		_, parseErr = repodata.ParsePrimary(open)
	case typeFilelists:
		_, parseErr = repodata.ParseFilelists(open)
	case typeOther:
		_, parseErr = repodata.ParseOther(open)
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
