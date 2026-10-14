package repo

import (
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"os/exec"

	"github.com/klauspost/compress/zstd"
)

// Compression selects the metadata compression algorithm.
type Compression string

const (
	// GZIP is the default; readable by every dnf on RHEL 8+.
	GZIP Compression = "gzip"
	// ZSTD produces smaller metadata but requires RHEL 8.4 or newer.
	ZSTD Compression = "zstd"
)

// ext returns the filename extension for the compression (e.g. ".gz").
func (c Compression) ext() string {
	switch c {
	case ZSTD:
		return ".zst"
	default:
		return ".gz"
	}
}

// compress returns the compressed form of data.
func (c Compression) compress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	switch c {
	case ZSTD:
		w, err := zstd.NewWriter(&buf)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			_ = w.Close()
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	case GZIP, "":
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(data); err != nil {
			_ = w.Close()
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("repo: unknown compression %q", c)
	}
	return buf.Bytes(), nil
}

// decompress detects the compression from the payload's magic bytes and returns
// the plain data. gzip and zstd are what this tool writes; xz and bzip2 are read
// as well because a repository created by other tooling (createrepo_c's --xz, an
// older createrepo) publishes its metadata that way, and taking such a
// repository over means reading it first. A payload with no recognized magic is
// returned unchanged (already plain XML).
func decompress(data []byte) ([]byte, error) {
	switch {
	case hasMagic(data, 0x1f, 0x8b):
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return io.ReadAll(zr)
	case hasMagic(data, 0x28, 0xb5, 0x2f, 0xfd):
		zr, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return io.ReadAll(zr)
	case hasMagic(data, 0xfd, '7', 'z', 'X', 'Z', 0x00):
		return xzDecompress(data)
	case hasMagic(data, 'B', 'Z', 'h'):
		return io.ReadAll(bzip2.NewReader(bytes.NewReader(data)))
	default:
		return data, nil
	}
}

// hasMagic reports whether data starts with the given bytes.
func hasMagic(data []byte, magic ...byte) bool {
	if len(data) < len(magic) {
		return false
	}
	for i, b := range magic {
		if data[i] != b {
			return false
		}
	}
	return true
}

// xzDecompress pipes data through the external xz command. No pure-Go xz
// decoder is bundled, so xz-compressed metadata needs the command; the error
// says so plainly rather than reporting a corrupt document.
func xzDecompress(data []byte) ([]byte, error) {
	if _, err := exec.LookPath("xz"); err != nil {
		return nil, fmt.Errorf("this repository's metadata is xz-compressed, which requires the 'xz' command; it was not found in PATH")
	}
	cmd := exec.Command("xz", "--decompress", "--stdout")
	cmd.Stdin = bytes.NewReader(data)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("xz --decompress: %w: %s", err, errBuf.String())
	}
	return out.Bytes(), nil
}
