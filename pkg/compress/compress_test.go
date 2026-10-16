package compress

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

const doc = "<metadata packages=\"1\"/>\n"

func encode(t *testing.T, f format) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.WriteCloser
	switch f {
	case gzipFormat:
		w = gzip.NewWriter(&buf)
	case zstdFormat:
		zw, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		w = zw
	case xzFormat:
		xw, err := xz.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		w = xw
	default:
		return []byte(doc)
	}
	if _, err := w.Write([]byte(doc)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Every format decodes in process, whatever the file is called: the content
// decides, so a misnamed file still reads.
func TestDecompressByContent(t *testing.T) {
	for _, f := range []format{plain, gzipFormat, zstdFormat, xzFormat} {
		data := encode(t, f)
		for _, name := range []string{"repodata/x-primary.xml.gz", "Packages", "Packages.xz"} {
			got, err := Decompress(name, data)
			if err != nil {
				t.Errorf("Decompress(%s as %q): %v", f, name, err)
				continue
			}
			if string(got) != doc {
				t.Errorf("Decompress(%s as %q) = %q, want %q", f, name, got, doc)
			}

			rc, err := DecompressStream(name, bytes.NewReader(data))
			if err != nil {
				t.Errorf("DecompressStream(%s as %q): %v", f, name, err)
				continue
			}
			got, err = io.ReadAll(rc)
			rc.Close()
			if err != nil || string(got) != doc {
				t.Errorf("DecompressStream(%s as %q) = %q, %v; want %q", f, name, got, err, doc)
			}
		}
	}
}

func TestDecompressStreamHandlesShortInput(t *testing.T) {
	rc, err := DecompressStream("Release", bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	if string(got) != "x" {
		t.Errorf("got %q, want %q", got, "x")
	}
}

// A limit stops decoding of a document that expands past it, whatever its
// format, so a small hostile file cannot fill memory.
func TestDecompressLimit(t *testing.T) {
	for _, f := range []format{plain, gzipFormat, zstdFormat, xzFormat} {
		data := encode(t, f)
		if out, err := DecompressLimit("doc", data, int64(len(doc))); err != nil || string(out) != doc {
			t.Errorf("%s at the limit: got (%q, %v), want the document", f, out, err)
		}
		if _, err := DecompressLimit("doc", data, int64(len(doc))-1); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s over the limit: got %v, want ErrTooLarge", f, err)
		}
	}
}
