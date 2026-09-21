package repo

import (
	"bytes"
	"compress/gzip"
	"os/exec"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// TestDecompressReadsForeignCompression covers the formats a repository this
// tool did not create can publish its metadata in. gzip and zstd are what it
// writes itself; xz and bzip2 exist only to be able to read someone else's
// repository, which is what a takeover starts with.
func TestDecompressReadsForeignCompression(t *testing.T) {
	plain := []byte(`<?xml version="1.0"?><metadata packages="0"/>`)

	cases := []struct {
		name    string
		encode  func(t *testing.T, data []byte) []byte
		skipMsg string
	}{
		{name: "plain", encode: func(_ *testing.T, d []byte) []byte { return d }},
		{name: "gzip", encode: func(t *testing.T, d []byte) []byte {
			var buf bytes.Buffer
			w := gzip.NewWriter(&buf)
			if _, err := w.Write(d); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			return buf.Bytes()
		}},
		{name: "zstd", encode: func(t *testing.T, d []byte) []byte {
			out, err := ZSTD.compress(d)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
		{name: "xz", encode: func(t *testing.T, d []byte) []byte { return pipeThrough(t, d, "xz", "--compress", "--stdout") }},
		{name: "bzip2", encode: func(t *testing.T, d []byte) []byte { return pipeThrough(t, d, "bzip2", "--compress", "--stdout") }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			encoded := c.encode(t, plain)
			got, err := decompress(encoded)
			if err != nil {
				t.Fatalf("decompress %s metadata: %v", c.name, err)
			}
			if !bytes.Equal(got, plain) {
				t.Errorf("decompressed %s metadata = %q, want %q", c.name, got, plain)
			}
		})
	}
}

// pipeThrough compresses data with an external command, skipping the test when
// the command is not installed.
func pipeThrough(t *testing.T, data []byte, name string, args ...string) []byte {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s is not installed", name)
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

// TestDecompressUnknownMagicIsLeftAlone documents the fallback: a document with
// no recognized magic is assumed to be the XML itself, which is how an
// uncompressed primary.xml is read.
func TestDecompressUnknownMagicIsLeftAlone(t *testing.T) {
	in := []byte("not compressed at all")
	got, err := decompress(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, in) {
		t.Errorf("decompress(%q) = %q, want it unchanged", in, got)
	}
}

// zstdMagic guards the assumption the detection relies on.
func TestZstdCompressionIsDetectable(t *testing.T) {
	out, err := ZSTD.compress([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zstd.NewReader(bytes.NewReader(out)); err != nil {
		t.Fatalf("zstd output is not readable: %v", err)
	}
	if !hasMagic(out, 0x28, 0xb5, 0x2f, 0xfd) {
		t.Error("zstd output does not carry the magic decompress detects")
	}
}
