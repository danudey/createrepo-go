package repo

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/compress"
	"github.com/danudey/createrepo-go/pkg/rpm/repodata"
)

// publishedRepo commits a one-package repository to a new directory and
// returns the directory and its parsed repomd.xml.
func publishedRepo(t *testing.T) (string, *repodata.Repomd) {
	t.Helper()
	dir := t.TempDir()
	r, _ := newLocalRepo(t, dir, true)
	if _, err := r.AddRPM(helloRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Close()
	data, err := os.ReadFile(filepath.Join(dir, repomdPath))
	if err != nil {
		t.Fatal(err)
	}
	md, err := repodata.ParseRepomd(data)
	if err != nil {
		t.Fatal(err)
	}
	return dir, md
}

func openErr(t *testing.T, dir string) error {
	t.Helper()
	be, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := OpenWith(context.Background(), be, Options{})
	if err == nil {
		r.Close()
	}
	return err
}

// repomd.xml is the only file a signature covers, so a primary.xml that does
// not match the checksum repomd.xml gives for it must be refused: otherwise a
// mirror can serve any package list under a correctly signed repomd.xml.
func TestOpenRefusesMetadataThatDoesNotMatchRepomd(t *testing.T) {
	dir, md := publishedRepo(t)
	var forged bytes.Buffer
	zw := gzip.NewWriter(&forged)
	zw.Write(repodata.WritePrimary(nil))
	zw.Close()
	primary := md.DataEntryByType("primary")
	if err := os.WriteFile(filepath.Join(dir, primary.Location), forged.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	err := openErr(t, dir)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Open of a repository with a substituted primary.xml = %v, want a checksum mismatch", err)
	}
}

// A document that decodes to more than repomd.xml's open-size is refused
// before it is all in memory.
func TestOpenBoundsDecompressionByOpenSize(t *testing.T) {
	dir, md := publishedRepo(t)
	for i := range md.Data {
		md.Data[i].OpenSize = 16
	}
	if err := os.WriteFile(filepath.Join(dir, repomdPath), repodata.WriteRepomd(md), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := openErr(t, dir); !errors.Is(err, compress.ErrTooLarge) {
		t.Fatalf("Open with an open-size smaller than the document = %v, want ErrTooLarge", err)
	}
}

func TestCheckDigest(t *testing.T) {
	data := []byte("hello\n")
	for typ, sum := range map[string]string{
		"sha":    "f572d396fae9206628714fb2ce00f72e94f2258f",
		"sha1":   "f572d396fae9206628714fb2ce00f72e94f2258f",
		"sha256": "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03",
		"SHA256": "5891B5B522D5DF086D0FF0B110FBD9D21BB4FC7163AF34D08286A2E846F6BE03",
		"sha512": "e7c22b994c59d9cf2b48e549b1e24666636045930d3da7c1acb299d1c3b7f931f94aae41edda2c2b207a36e10f8bcb8d45223e54878f5b316e7ce3b6bc019629",
	} {
		if err := checkDigest(typ, sum, data); err != nil {
			t.Errorf("checkDigest(%s) = %v, want nil", typ, err)
		}
	}
	if err := checkDigest("sha256", strings.Repeat("0", 64), data); err == nil {
		t.Error("checkDigest accepted a wrong digest")
	}
	if err := checkDigest("md5", "b1946ac92492d2347c6235b4d2611184", data); err == nil {
		t.Error("checkDigest accepted an unsupported checksum type")
	}
	if err := checkDigest("sha256", "", data); err == nil {
		t.Error("checkDigest accepted a missing checksum")
	}
}
