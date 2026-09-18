//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/danudey/createrepo-go/pkg/backend"
)

// openBackend opens an in-process backend for read-back inspection, closing it
// when the test ends. Used by the object-store harnesses (S3/GCS) whose storage
// is not directly visible on local disk.
func openBackend(t *testing.T, repoURL string) backend.Backend {
	t.Helper()
	be, err := backend.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatalf("open backend %s: %v", repoURL, err)
	}
	t.Cleanup(func() {
		if c, ok := be.(backend.Closer); ok {
			c.Close()
		}
	})
	return be
}

// beExists/beRead/beRemove/beWrite implement the inspection methods in terms of
// an in-process Backend (for S3/GCS).
func beExists(t *testing.T, repoURL, relpath string) bool {
	be := openBackend(t, repoURL)
	_, err := be.Stat(context.Background(), relpath)
	if errors.Is(err, backend.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatalf("stat %s: %v", relpath, err)
	}
	return true
}

func beRead(t *testing.T, repoURL, relpath string) []byte {
	be := openBackend(t, repoURL)
	rc, err := be.Get(context.Background(), relpath)
	if err != nil {
		t.Fatalf("get %s: %v", relpath, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", relpath, err)
	}
	return data
}

// beFetch reads an object via an in-process backend without a *testing.T, so it
// is safe to call from HTTP handler goroutines. Used by the S3/GCS harnesses.
func beFetch(repoURL, relpath string) ([]byte, bool, error) {
	be, err := backend.Open(context.Background(), repoURL)
	if err != nil {
		return nil, false, err
	}
	defer func() {
		if c, ok := be.(backend.Closer); ok {
			c.Close()
		}
	}()
	rc, err := be.Get(context.Background(), relpath)
	if errors.Is(err, backend.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// fileFetch reads a backing local file (local/sftp backends).
func fileFetch(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func beRemove(t *testing.T, repoURL, relpath string) {
	be := openBackend(t, repoURL)
	if err := be.Delete(context.Background(), relpath); err != nil {
		t.Fatalf("delete %s: %v", relpath, err)
	}
}

func beWrite(t *testing.T, repoURL, relpath string, data []byte) {
	be := openBackend(t, repoURL)
	if err := be.Put(context.Background(), relpath, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("put %s: %v", relpath, err)
	}
}

// ============================================================================
// local
// ============================================================================

type localHarness struct{ base string }

func (h *localHarness) Name() string { return "local" }

func (h *localHarness) Start(t *testing.T) { h.base = t.TempDir() }

func (h *localHarness) RepoURL(t *testing.T) string {
	dir := filepath.Join(h.base, uniq(t))
	must(t, os.MkdirAll(dir, 0o755))
	return dir
}

func (h *localHarness) Env() []string    { return nil }
func (h *localHarness) RemoteHash() bool { return true }

func (h *localHarness) abs(repoURL, relpath string) string {
	return filepath.Join(repoURL, filepath.FromSlash(relpath))
}

func (h *localHarness) Fetch(repoURL, relpath string) ([]byte, bool, error) {
	return fileFetch(h.abs(repoURL, relpath))
}

func (h *localHarness) Exists(t *testing.T, repoURL, relpath string) bool {
	_, err := os.Stat(h.abs(repoURL, relpath))
	return err == nil
}

func (h *localHarness) ReadAll(t *testing.T, repoURL, relpath string) []byte {
	data, err := os.ReadFile(h.abs(repoURL, relpath))
	must(t, err)
	return data
}

func (h *localHarness) Remove(t *testing.T, repoURL, relpath string) {
	must(t, os.Remove(h.abs(repoURL, relpath)))
}

func (h *localHarness) WriteRaw(t *testing.T, repoURL, relpath string, data []byte) {
	p := h.abs(repoURL, relpath)
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(t, os.WriteFile(p, data, 0o644))
}

// ============================================================================
// s3 (MinIO)
// ============================================================================

type s3Harness struct {
	bucket   string
	endpoint string
	cmd      *exec.Cmd
}

func (h *s3Harness) Name() string { return "s3-minio" }

func (h *s3Harness) Start(t *testing.T) {
	if _, ok := haveExec("minio"); !ok {
		t.Skip("minio binary not on PATH; skipping S3 backend")
	}
	port := freePort(t)
	h.endpoint = fmt.Sprintf("http://127.0.0.1:%d", port)
	h.bucket = "e2e"
	dataDir := t.TempDir()

	const ak, sk = "e2eaccesskey", "e2esecretkey"
	// Set process-wide so in-process read-back (backend.Open) works too.
	os.Setenv("AWS_ACCESS_KEY_ID", ak)
	os.Setenv("AWS_SECRET_ACCESS_KEY", sk)
	os.Setenv("AWS_REGION", "us-east-1")
	os.Setenv("AWS_ENDPOINT_URL", h.endpoint)

	h.cmd = exec.Command("minio", "server", dataDir, "--address", fmt.Sprintf("127.0.0.1:%d", port))
	h.cmd.Env = append(os.Environ(), "MINIO_ROOT_USER="+ak, "MINIO_ROOT_PASSWORD="+sk)
	logf, _ := os.Create(filepath.Join(t.TempDir(), "minio.log"))
	h.cmd.Stdout, h.cmd.Stderr = logf, logf
	if err := h.cmd.Start(); err != nil {
		t.Skipf("start minio: %v", err)
	}
	t.Cleanup(func() {
		if h.cmd.Process != nil {
			h.cmd.Process.Kill()
			h.cmd.Wait()
		}
	})

	// Wait for health, then create the bucket.
	waitFor(t, "minio health", 20*time.Second, func() error {
		resp, err := http.Get(h.endpoint + "/minio/health/live")
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return nil
	})
	h.createBucket(t)
}

func (h *s3Harness) createBucket(t *testing.T) {
	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	must(t, err)
	cl := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(h.endpoint)
		o.UsePathStyle = true
	})
	_, err = cl.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(h.bucket)})
	if err != nil && !strings.Contains(err.Error(), "BucketAlreadyOwnedByYou") {
		must(t, err)
	}
}

func (h *s3Harness) RepoURL(t *testing.T) string {
	return fmt.Sprintf("s3://%s/%s", h.bucket, uniq(t))
}

func (h *s3Harness) Env() []string {
	return []string{
		"AWS_ACCESS_KEY_ID=" + os.Getenv("AWS_ACCESS_KEY_ID"),
		"AWS_SECRET_ACCESS_KEY=" + os.Getenv("AWS_SECRET_ACCESS_KEY"),
		"AWS_REGION=us-east-1",
		"AWS_ENDPOINT_URL=" + h.endpoint,
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
}
func (h *s3Harness) RemoteHash() bool { return true }

func (h *s3Harness) Fetch(repoURL, relpath string) ([]byte, bool, error) {
	return beFetch(repoURL, relpath)
}

func (h *s3Harness) Exists(t *testing.T, repoURL, relpath string) bool {
	return beExists(t, repoURL, relpath)
}

func (h *s3Harness) ReadAll(t *testing.T, repoURL, relpath string) []byte {
	return beRead(t, repoURL, relpath)
}
func (h *s3Harness) Remove(t *testing.T, repoURL, relpath string) { beRemove(t, repoURL, relpath) }
func (h *s3Harness) WriteRaw(t *testing.T, repoURL, relpath string, data []byte) {
	beWrite(t, repoURL, relpath, data)
}

// ============================================================================
// sftp (local sshd + in-process ssh-agent)
// ============================================================================

type sftpHarness struct {
	port int
	user string
	root string // local directory served as the SSH file system
	home string // fake HOME (carries our known_hosts) for the CLI
	sock string // in-process ssh-agent socket
	cmd  *exec.Cmd
	stop func()
}

func (h *sftpHarness) Name() string { return "sftp" }

func sftpServerPath() (string, bool) {
	for _, p := range []string{
		"/usr/lib/openssh/sftp-server",
		"/usr/libexec/openssh/sftp-server",
		"/usr/libexec/sftp-server",
		"/usr/lib/ssh/sftp-server",
	} {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

func (h *sftpHarness) Start(t *testing.T) {
	if missing, ok := haveExec("sshd", "ssh-keygen"); !ok {
		t.Skipf("%s not available; skipping SFTP backend", missing)
	}
	sftpSrv, ok := sftpServerPath()
	if !ok {
		t.Skip("sftp-server binary not found; skipping SFTP backend")
	}
	h.user = os.Getenv("USER")
	if h.user == "" {
		t.Skip("USER not set; skipping SFTP backend")
	}
	h.port = freePort(t)
	h.root = t.TempDir()
	keyDir := t.TempDir()
	h.home = t.TempDir()

	// Host + user keys.
	hostKey := filepath.Join(keyDir, "host")
	userKey := filepath.Join(keyDir, "id")
	runTool(t, "ssh-keygen", "-q", "-t", "ed25519", "-f", hostKey, "-N", "")
	runTool(t, "ssh-keygen", "-q", "-t", "ed25519", "-f", userKey, "-N", "")

	authKeys := filepath.Join(keyDir, "authorized_keys")
	cp(t, userKey+".pub", authKeys)

	// known_hosts so the CLI verifies the ephemeral host key rather than failing.
	hostPub := readFile(t, hostKey+".pub")
	sshDir := filepath.Join(h.home, ".ssh")
	must(t, os.MkdirAll(sshDir, 0o700))
	known := fmt.Sprintf("[127.0.0.1]:%d %s\n", h.port, strings.TrimSpace(string(hostPub)))
	must(t, os.WriteFile(filepath.Join(sshDir, "known_hosts"), []byte(known), 0o600))

	cfg := filepath.Join(keyDir, "sshd_config")
	must(t, os.WriteFile(cfg, []byte(fmt.Sprintf(
		"Port %d\nListenAddress 127.0.0.1\nHostKey %s\nAuthorizedKeysFile %s\n"+
			"UsePAM no\nPasswordAuthentication no\nPubkeyAuthentication yes\n"+
			"StrictModes no\nPidFile %s/sshd.pid\nSubsystem sftp %s\n",
		h.port, hostKey, authKeys, keyDir, sftpSrv)), 0o600))

	// In-process ssh-agent serving userKey.
	h.sock = filepath.Join(keyDir, "agent.sock")
	h.stop = startAgent(t, h.sock, userKey)

	logPath := filepath.Join(keyDir, "sshd.log")
	h.cmd = exec.Command("/usr/sbin/sshd", "-D", "-f", cfg, "-E", logPath)
	if err := h.cmd.Start(); err != nil {
		t.Skipf("start sshd: %v", err)
	}
	t.Cleanup(func() {
		if h.cmd.Process != nil {
			h.cmd.Process.Kill()
			h.cmd.Wait()
		}
		if h.stop != nil {
			h.stop()
		}
	})
	waitForTCP(t, fmt.Sprintf("127.0.0.1:%d", h.port), 15*time.Second)
}

func (h *sftpHarness) RepoURL(t *testing.T) string {
	dir := filepath.Join(h.root, uniq(t))
	must(t, os.MkdirAll(dir, 0o755))
	return fmt.Sprintf("sftp://%s@127.0.0.1:%d%s", h.user, h.port, dir)
}

func (h *sftpHarness) Env() []string {
	return []string{
		"SSH_AUTH_SOCK=" + h.sock,
		"HOME=" + h.home,
		"USER=" + h.user,
		"PATH=" + os.Getenv("PATH"),
	}
}
func (h *sftpHarness) RemoteHash() bool { return true }

// localPath maps an sftp:// repo URL + relpath to the backing local file.
func (h *sftpHarness) localPath(t *testing.T, repoURL, relpath string) string {
	u, err := url.Parse(repoURL)
	must(t, err)
	return filepath.Join(u.Path, filepath.FromSlash(relpath))
}

func (h *sftpHarness) Fetch(repoURL, relpath string) ([]byte, bool, error) {
	u, err := url.Parse(repoURL)
	if err != nil {
		return nil, false, err
	}
	return fileFetch(filepath.Join(u.Path, filepath.FromSlash(relpath)))
}

func (h *sftpHarness) Exists(t *testing.T, repoURL, relpath string) bool {
	_, err := os.Stat(h.localPath(t, repoURL, relpath))
	return err == nil
}

func (h *sftpHarness) ReadAll(t *testing.T, repoURL, relpath string) []byte {
	data, err := os.ReadFile(h.localPath(t, repoURL, relpath))
	must(t, err)
	return data
}

func (h *sftpHarness) Remove(t *testing.T, repoURL, relpath string) {
	must(t, os.Remove(h.localPath(t, repoURL, relpath)))
}

func (h *sftpHarness) WriteRaw(t *testing.T, repoURL, relpath string, data []byte) {
	p := h.localPath(t, repoURL, relpath)
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(t, os.WriteFile(p, data, 0o644))
}

// ============================================================================
// gcs (fake-gcs-server container)
// ============================================================================

type gcsHarness struct {
	bucket    string
	endpoint  string // host:port
	container string
}

func (h *gcsHarness) Name() string { return "gcs-fake" }

func (h *gcsHarness) Start(t *testing.T) {
	if _, ok := haveExec("docker"); !ok {
		t.Skip("docker not available; skipping GCS backend")
	}
	port := freePort(t)
	h.endpoint = fmt.Sprintf("127.0.0.1:%d", port)
	h.bucket = "e2e"
	h.container = "cr-e2e-gcs-" + uniq(t)

	args := []string{
		"run", "-d", "--name", h.container,
		"-p", fmt.Sprintf("%d:4443", port),
		"fsouza/fake-gcs-server:latest",
		"-scheme", "http", "-port", "4443",
		"-public-host", h.endpoint,
	}
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Skipf("docker run fake-gcs-server: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", h.container).Run() })

	os.Setenv("STORAGE_EMULATOR_HOST", h.endpoint)

	// Wait for the storage API, then create the bucket.
	base := "http://" + h.endpoint
	waitFor(t, "fake-gcs", 25*time.Second, func() error {
		resp, err := http.Get(base + "/storage/v1/b?project=test")
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return nil
	})
	resp, err := http.Post(base+"/storage/v1/b?project=test",
		"application/json", strings.NewReader(`{"name":"`+h.bucket+`"}`))
	must(t, err)
	resp.Body.Close()
}

func (h *gcsHarness) RepoURL(t *testing.T) string {
	return fmt.Sprintf("gs://%s/%s", h.bucket, uniq(t))
}

func (h *gcsHarness) Env() []string {
	return []string{
		"STORAGE_EMULATOR_HOST=" + h.endpoint,
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
}
func (h *gcsHarness) RemoteHash() bool { return true }

func (h *gcsHarness) Fetch(repoURL, relpath string) ([]byte, bool, error) {
	return beFetch(repoURL, relpath)
}

func (h *gcsHarness) Exists(t *testing.T, repoURL, relpath string) bool {
	return beExists(t, repoURL, relpath)
}

func (h *gcsHarness) ReadAll(t *testing.T, repoURL, relpath string) []byte {
	return beRead(t, repoURL, relpath)
}
func (h *gcsHarness) Remove(t *testing.T, repoURL, relpath string) { beRemove(t, repoURL, relpath) }
func (h *gcsHarness) WriteRaw(t *testing.T, repoURL, relpath string, data []byte) {
	beWrite(t, repoURL, relpath, data)
}

// ---- small file/exec helpers used by the harnesses -------------------------

func runTool(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func cp(t *testing.T, src, dst string) {
	t.Helper()
	data := readFile(t, src)
	must(t, os.WriteFile(dst, data, 0o644))
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	data, err := os.ReadFile(p)
	must(t, err)
	return data
}
