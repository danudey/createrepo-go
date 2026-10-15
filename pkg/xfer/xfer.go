// Package xfer moves repository files between backends and the local disk: the
// transfer plumbing createrepo-go and createapt-go share. It knows nothing about
// either repository format. A format describes its files as Objects — path,
// size, checksum, and whether the content can change without its length
// changing — and xfer fetches, verifies, uploads and replicates them.
package xfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/progress"
)

// Phase labels shown on a transfer's progress bar.
const (
	PhaseDownload = "download"
	PhaseUpload   = "upload"
)

// TempSuffix reduces a repository path to something os.CreateTemp will accept
// at the tail of a pattern. A path comes from a remote repository's metadata,
// so it holds whatever that publisher wrote; os.CreateTemp rejects a pattern
// containing a path separator, and on Windows a backslash is one, so path.Base
// alone is not enough.
func TempSuffix(relpath string) string {
	base := path.Base(relpath)
	if i := strings.LastIndexAny(base, `\/`); i >= 0 {
		base = base[i+1:]
	}
	switch base {
	case "", ".", "..":
		return "obj"
	}
	return base
}

// createTemp makes a temporary file named after relpath, so a leftover one is
// recognizable.
func createTemp(prefix, relpath string) (*os.File, error) {
	return os.CreateTemp("", prefix+"-*-"+TempSuffix(relpath))
}

// GetToTemp streams relpath from be into a new temporary file and returns its
// path. The caller owns the file and must remove it. item, which may be nil, is
// credited with the bytes as they arrive.
func GetToTemp(ctx context.Context, be backend.Backend, relpath, prefix string, item *progress.Item) (string, error) {
	rc, err := be.Get(ctx, relpath)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	f, err := createTemp(prefix, relpath)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(item.Writer(f), rc); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// LocalCopy makes relpath readable as a local file. A backend that keeps its
// objects as plain files hands back the file itself and a no-op cleanup,
// reporting 0 bytes transferred; any other backend downloads to a temporary
// file, released by the returned cleanup, and reports what that cost.
func LocalCopy(ctx context.Context, be backend.Backend, relpath, prefix string, item *progress.Item) (local string, cleanup func(), transferred int64, err error) {
	noop := func() {}
	if p, ok := backend.LocalPath(be, relpath); ok {
		// A FileStore reports the path of an object whether or not it exists,
		// so absence has to be established separately.
		if _, err := os.Stat(p); err != nil {
			if os.IsNotExist(err) {
				return "", noop, 0, backend.ErrNotExist
			}
			return "", noop, 0, err
		}
		return p, noop, 0, nil
	}
	tmp, err := GetToTemp(ctx, be, relpath, prefix, item)
	if err != nil {
		return "", noop, 0, err
	}
	fi, err := os.Stat(tmp)
	if err != nil {
		os.Remove(tmp)
		return "", noop, 0, err
	}
	return tmp, func() { os.Remove(tmp) }, fi.Size(), nil
}

// PutFile uploads a local file to dest. item, which may be nil, is credited
// with the bytes as they go out.
func PutFile(ctx context.Context, be backend.Backend, dest, local string, item *progress.Item) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	return be.Put(ctx, dest, item.Reader(f), fi.Size())
}

// PutBytes writes data to dest. The body is seekable, so an object store can
// hash and sign it in place.
func PutBytes(ctx context.Context, be backend.Backend, dest string, data []byte, item *progress.Item) error {
	return be.Put(ctx, dest, item.Reader(bytes.NewReader(data)), int64(len(data)))
}

// ReadAll fetches relpath into memory. It is meant for metadata, which is small.
func ReadAll(ctx context.Context, be backend.Backend, relpath string) ([]byte, error) {
	rc, err := be.Get(ctx, relpath)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// HashObject streams relpath through sha256, returning the hex digest and the
// number of bytes read. Nothing is buffered: a package of any size costs one
// pass and no disk.
func HashObject(ctx context.Context, be backend.Backend, relpath string, item *progress.Item) (string, int64, error) {
	return HashObjectWith(ctx, be, relpath, item, nil)
}

// HashObjectWith is HashObject that also hands the bytes to inspect as they
// stream past, so a caller that needs to look inside the object pays for one
// transfer, not two. inspect may stop reading early; the rest is still hashed.
// A nil inspect is HashObject.
func HashObjectWith(ctx context.Context, be backend.Backend, relpath string, item *progress.Item, inspect func(io.Reader) error) (string, int64, error) {
	rc, err := be.Get(ctx, relpath)
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()
	h := sha256.New()
	cw := &countWriter{w: item.Writer(h)}
	src := io.TeeReader(rc, cw)
	if inspect != nil {
		if err := inspect(src); err != nil {
			return "", cw.n, err
		}
	}
	if _, err := io.Copy(io.Discard, src); err != nil {
		return "", cw.n, err
	}
	return hex.EncodeToString(h.Sum(nil)), cw.n, nil
}

// countWriter counts the bytes written through it.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// ServerCopy duplicates from to to within be without moving the bytes through
// this process. It is only for plans that established the backend is a Copier.
func ServerCopy(ctx context.Context, be backend.Backend, from, to string) error {
	c, ok := be.(backend.Copier)
	if !ok {
		return fmt.Errorf("backend %s does not support server-side copy", be)
	}
	return c.Copy(ctx, from, to)
}
