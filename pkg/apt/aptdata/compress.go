package aptdata

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/danudey/createrepo-go/pkg/compress"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// The filename extensions the compressed forms use.
const (
	extGZIP = ".gz"
	extXZ   = ".xz"
	extZSTD = ".zst"
	extBZ2  = ".bz2"
)

// Compression names one of the forms an index is published in. An index is
// normally written in several at once, because which one a client fetches
// depends on its apt version.
type Compression string

// The compression forms this tool writes.
const (
	// None publishes the index uncompressed. apt still accepts it, and it is
	// what a by-hash fallback and a human reading the repository want.
	None Compression = "none"
	// GZIP is understood by every apt ever shipped.
	GZIP Compression = "gzip"
	// XZ is what Debian and Ubuntu publish; apt has read it since 0.9.
	XZ Compression = "xz"
	// ZSTD is read by apt 2.3+ (Ubuntu 22.04, Debian 12) and nothing older.
	ZSTD Compression = "zstd"
)

// DefaultCompressions is the set written when none is chosen: the plain index
// plus gzip and xz, which between them cover every apt in service.
var DefaultCompressions = []Compression{None, GZIP, XZ}

// Ext returns the filename extension for the compression, empty for None.
func (c Compression) Ext() string {
	switch c {
	case GZIP:
		return extGZIP
	case XZ:
		return extXZ
	case ZSTD:
		return extZSTD
	default:
		return ""
	}
}

// Valid reports whether c names a supported compression.
func (c Compression) Valid() bool {
	switch c {
	case None, GZIP, XZ, ZSTD:
		return true
	default:
		return false
	}
}

// ParseCompressions turns a comma-separated list such as "gzip,xz" into a
// normalized, deduplicated list. The uncompressed form is always included: it
// is the only one every client and every mirroring tool can read, and it costs
// one small object per index.
func ParseCompressions(s string) ([]Compression, error) {
	seen := map[Compression]bool{None: true}
	for _, part := range strings.Split(s, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		switch name {
		case "":
			continue
		case "gz":
			name = string(GZIP)
		case "zst":
			name = string(ZSTD)
		case "plain", "uncompressed":
			name = string(None)
		}
		c := Compression(name)
		if !c.Valid() {
			return nil, fmt.Errorf("unknown compression %q (valid: none, gzip, xz, zstd)", part)
		}
		seen[c] = true
	}
	var out []Compression
	for _, c := range []Compression{None, GZIP, XZ, ZSTD} {
		if seen[c] {
			out = append(out, c)
		}
	}
	return out, nil
}

// Compress encodes data in the named form. None returns the input unchanged.
func (c Compression) Compress(data []byte) ([]byte, error) {
	switch c {
	case None:
		return data, nil
	case GZIP:
		var buf bytes.Buffer
		// Best compression: indexes are written once and fetched by every
		// client, so the extra CPU on publish is repaid many times over.
		w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case XZ:
		var buf bytes.Buffer
		w, err := xz.NewWriter(&buf)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case ZSTD:
		var buf bytes.Buffer
		w, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	default:
		return nil, fmt.Errorf("unsupported compression %q", c)
	}
}

// Decompress decodes an index, whatever its compression; uncompressed data is
// returned unchanged. See package compress.
func Decompress(name string, data []byte) ([]byte, error) {
	return compress.Decompress(name, data)
}

// MaxIndexSize bounds any one index or Release this tool reads, compressed or
// decoded, when nothing smaller is known. Debian's largest Packages index is
// well under 100 MiB; the bound exists so a hostile repository cannot make a
// small file expand until memory runs out.
const MaxIndexSize = 1 << 30

// ErrTooLarge is returned for a document larger than the limit it was read
// with.
var ErrTooLarge = errors.New("larger than the limit")

// DecompressLimit is Decompress with a bound: decoding stops with ErrTooLarge
// once the output passes limit bytes.
func DecompressLimit(name string, data []byte, limit int64) ([]byte, error) {
	rc, err := compress.DecompressStream(name, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return ReadLimit(name, rc, limit)
}

// ReadLimit reads r to the end, failing with ErrTooLarge if it holds more
// than limit bytes.
func ReadLimit(name string, r io.Reader, limit int64) ([]byte, error) {
	// One byte past the limit tells "exactly at" from "over".
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s: %w (%d bytes)", name, ErrTooLarge, limit)
	}
	return data, nil
}

// DecodedLimit is the most an index at path may decode to: the size the
// Release records for its uncompressed form, which a Release lists alongside
// the compressed ones, or MaxIndexSize when it records none.
func (r *Release) DecodedLimit(path string) int64 {
	plain := path
	for _, ext := range []string{extGZIP, extXZ, extZSTD, extBZ2, ".zstd", ".lzma"} {
		if strings.HasSuffix(plain, ext) {
			plain = strings.TrimSuffix(plain, ext)
			break
		}
	}
	for _, f := range r.Files {
		if f.Path == plain && f.Size > 0 && f.Size < MaxIndexSize {
			return f.Size
		}
	}
	return MaxIndexSize
}

// DecompressStream wraps r with the decoder its content calls for. The caller
// closes the returned reader. It is used where an index is large enough that
// buffering it whole would be wasteful.
func DecompressStream(name string, r io.Reader) (io.ReadCloser, error) {
	return compress.DecompressStream(name, r)
}
