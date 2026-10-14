// Package compress decodes the compressed metadata both repository formats
// publish. Which tool wrote a repository decides its compression — createrepo_c
// may use xz or bzip2, apt indexes come in gzip, xz and zstd — so reading one
// means accepting all of them.
//
// Nothing here shells out: gzip, bzip2, xz and zstd are all decoded in process,
// so a repository can be read on a host with no compression utilities
// installed.
//
// Writing stays with each format, which publishes a different set.
package compress

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// format is a compression this package decodes.
type format int

const (
	plain format = iota
	gzipFormat
	zstdFormat
	xzFormat
	bzip2Format
)

func (f format) String() string {
	switch f {
	case gzipFormat:
		return "gzip"
	case zstdFormat:
		return "zstd"
	case xzFormat:
		return "xz"
	case bzip2Format:
		return "bzip2"
	default:
		return "plain"
	}
}

// magics are the leading bytes of each format. The content decides first: a
// file's name is only what its publisher called it.
var magics = []struct {
	f     format
	magic []byte
}{
	{gzipFormat, []byte{0x1f, 0x8b}},
	{zstdFormat, []byte{0x28, 0xb5, 0x2f, 0xfd}},
	{xzFormat, []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}},
	{bzip2Format, []byte{'B', 'Z', 'h'}},
}

// maxMagic is the longest magic, and so how much of a stream has to be seen.
const maxMagic = 6

// detect names the format of a payload starting with head. Only the content
// decides: a file's name is just what its publisher called it, and a payload
// that is already plain under a compressed name is common — an HTTP server
// that marks a .gz file Content-Encoding: gzip has it decoded in transit.
// Anything without a known magic is plain; if it is in fact damaged, the parser
// that reads it next says so.
func detect(head []byte) format {
	for _, m := range magics {
		if bytes.HasPrefix(head, m.magic) {
			return m.f
		}
	}
	return plain
}

// Decompress decodes data, a whole compressed document; uncompressed data is
// returned unchanged. name is its path in the repository, for error messages.
func Decompress(name string, data []byte) ([]byte, error) {
	f := detect(data)
	if f == plain {
		return data, nil
	}
	r, err := open(f, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", f, name, err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", f, name, err)
	}
	return out, nil
}

// DecompressStream wraps r with the decoder its content calls for. The caller
// closes the returned reader. It is for documents large enough that buffering
// them whole would be wasteful.
func DecompressStream(name string, r io.Reader) (io.ReadCloser, error) {
	br := bufio.NewReaderSize(r, 4096)
	head, err := br.Peek(maxMagic)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, err
	}
	f := detect(head)
	if f == plain {
		return io.NopCloser(br), nil
	}
	rc, err := open(f, br)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", f, name, err)
	}
	return rc, nil
}

// open starts decoding r as f.
func open(f format, r io.Reader) (io.ReadCloser, error) {
	switch f {
	case gzipFormat:
		return gzip.NewReader(r)
	case zstdFormat:
		zr, err := zstd.NewReader(r)
		if err != nil {
			return nil, err
		}
		return zr.IOReadCloser(), nil
	case xzFormat:
		xr, err := xz.NewReader(r)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(xr), nil
	case bzip2Format:
		return io.NopCloser(bzip2.NewReader(r)), nil
	default:
		return io.NopCloser(r), nil
	}
}
