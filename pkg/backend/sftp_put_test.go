package backend

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkg/sftp"
)

// fakeFS is an SFTP server's rename behaviour: without posix-rename, rename
// refuses to overwrite, as SFTP v3 specifies.
type fakeFS struct {
	files     map[string]string
	posix     bool
	failPosix bool
	// failRenameTo makes the next rename onto this name fail even when it is
	// free (once: the restore that follows must work).
	failRenameTo string
}

func (f *fakeFS) HasExtension(name string) (string, bool) {
	return "1", f.posix && name == "posix-rename@openssh.com"
}

func (f *fakeFS) PosixRename(from, to string) error {
	if f.failPosix {
		return errors.New("no space left on device")
	}
	f.files[to] = f.files[from]
	delete(f.files, from)
	return nil
}

func (f *fakeFS) Rename(from, to string) error {
	if _, exists := f.files[to]; exists {
		return errors.New("failure")
	}
	if to == f.failRenameTo {
		f.failRenameTo = ""
		return errors.New("failure")
	}
	if _, ok := f.files[from]; !ok {
		return os.ErrNotExist
	}
	f.files[to] = f.files[from]
	delete(f.files, from)
	return nil
}

func (f *fakeFS) Remove(p string) error {
	delete(f.files, p)
	return nil
}

// A failed replacement must leave the published file in place: before, any
// rename error deleted it and tried again, so a full disk or a dropped
// connection left repomd.xml or Release missing.
func TestReplaceFileNeverLosesTheLiveFile(t *testing.T) {
	t.Run("posix-rename fails", func(t *testing.T) {
		fs := &fakeFS{files: map[string]string{"repomd.xml": "old", "tmp": "new"}, posix: true, failPosix: true}
		if err := replaceFile(fs, "tmp", "repomd.xml", "x"); err == nil {
			t.Error("a failed posix-rename was reported as success")
		}
		if fs.files["repomd.xml"] != "old" {
			t.Errorf("repomd.xml = %q after a failed replace, want the old file", fs.files["repomd.xml"])
		}
	})
	t.Run("v3 rename fails after moving the old file aside", func(t *testing.T) {
		fs := &fakeFS{files: map[string]string{"repomd.xml": "old", "tmp": "new"}, failRenameTo: "repomd.xml"}
		if err := replaceFile(fs, "tmp", "repomd.xml", "x"); err == nil {
			t.Error("a failed rename was reported as success")
		}
		if fs.files["repomd.xml"] != "old" {
			t.Errorf("repomd.xml = %q after a failed replace, want the old file restored", fs.files["repomd.xml"])
		}
	})
	t.Run("v3 rename succeeds", func(t *testing.T) {
		fs := &fakeFS{files: map[string]string{"repomd.xml": "old", "tmp": "new"}}
		if err := replaceFile(fs, "tmp", "repomd.xml", "x"); err != nil {
			t.Fatal(err)
		}
		if len(fs.files) != 1 || fs.files["repomd.xml"] != "new" {
			t.Errorf("files after replace = %v, want only the new repomd.xml", fs.files)
		}
	})
}

// newPipeSFTP serves a temporary directory over SFTP in process and returns a
// backend connected to it.
func newPipeSFTP(t *testing.T) (*sftpBackend, string) {
	t.Helper()
	root := t.TempDir()
	serverConn, clientConn := net.Pipe()
	srv, err := sftp.NewServer(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	client, err := sftp.NewClientPipe(clientConn, clientConn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); srv.Close() })
	return &sftpBackend{client: client, root: filepath.ToSlash(root), label: "sftp://test"}, root
}

// gatedReader yields its first half, then waits for release before the rest.
type gatedReader struct {
	data    string
	pos     int
	halfway chan struct{}
	release chan struct{}
}

func (g *gatedReader) Read(p []byte) (int, error) {
	if g.pos == len(g.data)/2 && g.halfway != nil {
		close(g.halfway)
		g.halfway = nil
		<-g.release
	}
	if g.pos >= len(g.data) {
		return 0, io.EOF
	}
	end := len(g.data)
	if g.pos < len(g.data)/2 {
		end = len(g.data) / 2
	}
	n := copy(p, g.data[g.pos:end])
	g.pos += n
	return n, nil
}

// Two publishers writing the same file at once must each produce a whole
// file. Before, both uploaded to the same fixed temp name, so one wrote into
// the other's upload, and the loser's failed rename deleted the published file.
func TestSFTPPutsToTheSameFileDoNotCollide(t *testing.T) {
	be, root := newPipeSFTP(t)
	ctx := context.Background()
	first := strings.Repeat("A", 64*1024)
	second := strings.Repeat("B", 64*1024)

	halfway, release := make(chan struct{}), make(chan struct{})
	slow := &gatedReader{data: first, halfway: halfway, release: release}
	done := make(chan error, 1)
	go func() { done <- be.Put(ctx, "repodata/repomd.xml", slow, int64(len(first))) }()
	<-halfway
	if err := be.Put(ctx, "repodata/repomd.xml", strings.NewReader(second), int64(len(second))); err != nil {
		t.Fatalf("the second Put: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the first Put: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, "repodata", "repomd.xml"))
	if err != nil {
		t.Fatalf("repomd.xml after two Puts: %v", err)
	}
	if string(got) != first && string(got) != second {
		t.Errorf("repomd.xml is a mix of both uploads (%d bytes)", len(got))
	}
	left, _ := filepath.Glob(filepath.Join(root, "repodata", "*.cr*"))
	if len(left) > 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}
