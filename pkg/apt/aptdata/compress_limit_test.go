package aptdata

import (
	"errors"
	"strings"
	"testing"
)

func TestDecompressLimit(t *testing.T) {
	doc := []byte(strings.Repeat("Package: hello\n\n", 64))
	for _, c := range []Compression{None, GZIP, XZ, ZSTD} {
		data, err := c.Compress(doc)
		if err != nil {
			t.Fatal(err)
		}
		if out, err := DecompressLimit("Packages", data, int64(len(doc))); err != nil || string(out) != string(doc) {
			t.Errorf("%s at the limit: got (%d bytes, %v), want the document", c, len(out), err)
		}
		if _, err := DecompressLimit("Packages", data, int64(len(doc))-1); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s over the limit: got %v, want ErrTooLarge", c, err)
		}
	}
}

func TestDecodedLimit(t *testing.T) {
	rel := &Release{Files: []IndexFile{
		{Path: "main/binary-amd64/Packages", Size: 1234},
		{Path: "main/binary-amd64/Packages.xz", Size: 200},
		{Path: "main/source/Sources.gz", Size: 300},
	}}
	for path, want := range map[string]int64{
		"main/binary-amd64/Packages.xz": 1234,
		"main/binary-amd64/Packages":    1234,
		"main/source/Sources.gz":        MaxIndexSize, // no uncompressed entry
	} {
		if got := rel.DecodedLimit(path); got != want {
			t.Errorf("DecodedLimit(%q) = %d, want %d", path, got, want)
		}
	}
}
