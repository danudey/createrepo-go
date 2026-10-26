package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// local is a Backend backed by a directory on the local filesystem.
type local struct {
	root string
}

func newLocal(root string) (*local, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &local{root: abs}, nil
}

// path maps an object path onto the filesystem, refusing one that would land
// outside the root (see ErrUnsafePath). A leading slash means the root.
func (l *local) path(relpath string) (string, error) {
	rel := filepath.FromSlash(strings.TrimLeft(relpath, "/"))
	if rel != "" && !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%w: %q", ErrUnsafePath, relpath)
	}
	return filepath.Join(l.root, rel), nil
}

// LocalPath implements FileStore: this backend's objects are plain files, so a
// caller can open one directly rather than transferring a copy.
func (l *local) LocalPath(relpath string) string {
	p, err := l.path(relpath)
	if err != nil {
		return ""
	}
	return p
}

func (l *local) Get(_ context.Context, relpath string) (io.ReadCloser, error) {
	p, err := l.path(relpath)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return nil, ErrNotExist
	}
	return f, err
}

func (l *local) Stat(_ context.Context, relpath string) (*FileInfo, error) {
	p, err := l.path(relpath)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(p)
	if os.IsNotExist(err) {
		return nil, ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	return &FileInfo{Size: fi.Size()}, nil
}

func (l *local) Put(_ context.Context, relpath string, r io.Reader, _ int64) error {
	dst, err := l.path(relpath)
	if err != nil {
		return err
	}
	// #nosec G301 -- repository directories are served to clients over HTTP
	// and must stay world-readable; 0750 would break the published repo.
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// Write to a temp file in the same directory, then rename for atomicity.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".crtmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	// CreateTemp makes the file 0600, and the rename keeps that mode, so
	// without this every published file is readable by its owner alone and a
	// web server serving the repository answers 403. Files get the same
	// world-readable treatment as the directories above.
	// #nosec G302 -- published repository files must be world-readable.
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	// Flush before the rename, so a crash cannot leave a renamed but empty
	// file (repomd.xml or Release) where the old one was.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

func (l *local) Delete(_ context.Context, relpath string) error {
	p, err := l.path(relpath)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (l *local) String() string { return l.root }

// Copy duplicates src to dst on the local filesystem, reusing Put's atomic
// temp-file-and-rename so a reader never observes a partial destination.
func (l *local) Copy(ctx context.Context, src, dst string) error {
	p, err := l.path(src)
	if err != nil {
		return err
	}
	in, err := os.Open(p)
	if os.IsNotExist(err) {
		return ErrNotExist
	}
	if err != nil {
		return err
	}
	defer drainClose(in)
	return l.Put(ctx, dst, in, -1)
}

// List implements Lister by walking the directory tree rooted at prefix,
// returning each regular file as a repo-relative, forward-slash path. A missing
// prefix directory yields an empty list (an empty repository is not an error).
func (l *local) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	root, err := l.path(prefix)
	if err != nil {
		return nil, err
	}
	var out []ObjectInfo
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && p == root {
				return nil // nothing under this prefix yet
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(l.root, p)
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, ObjectInfo{Path: filepath.ToSlash(rel), Size: fi.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// HashesContent reports that Hash reads the file itself.
func (l *local) HashesContent() bool { return true }

// Hash implements RemoteHasher by hashing the local file.
func (l *local) Hash(_ context.Context, relpath, algo string) (string, bool, error) {
	if algo != AlgoSHA256 {
		return "", false, fmt.Errorf("local: unsupported hash %q", algo)
	}
	p, err := l.path(relpath)
	if err != nil {
		return "", false, err
	}
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return "", false, ErrNotExist
	}
	if err != nil {
		return "", false, err
	}
	defer drainClose(f)
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", false, err
	}
	return hex.EncodeToString(h.Sum(nil)), true, nil
}
