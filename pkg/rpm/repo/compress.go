package repo

import (
	"bytes"
	"compress/gzip"
	"fmt"

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
