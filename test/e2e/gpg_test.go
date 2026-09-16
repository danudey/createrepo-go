//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gpgKey is an ephemeral signing key in its own GnuPG home, with exported
// public/secret key files for the CLI's --gpg-key / --keyring flags.
type gpgKey struct {
	home    string // GNUPGHOME
	keyID   string // fingerprint
	pubFile string // exported public key (armored)
	secFile string // exported secret key (armored)
}

// env returns the environment variable needed to use this key's keyring.
func (k gpgKey) env() []string { return []string{"GNUPGHOME=" + k.home} }

// setupGPGKey creates a throwaway signing key, skipping the test if the GPG
// tooling is unavailable.
func setupGPGKey(t *testing.T) gpgKey {
	t.Helper()
	if missing, ok := haveExec("gpg", "rpmsign", "rpmkeys"); !ok {
		t.Skipf("%s not available; skipping signing scenario", missing)
	}
	return newGPGKey(t)
}

// maybeGPGKey creates a signing key if the GPG tooling is present, returning
// (key, true). When the tooling is absent it returns (zero, false) instead of
// skipping, so callers can fall back to an unsigned repository.
func maybeGPGKey(t *testing.T) (gpgKey, bool) {
	t.Helper()
	if _, ok := haveExec("gpg", "rpmsign", "rpmkeys"); !ok {
		return gpgKey{}, false
	}
	return newGPGKey(t), true
}

func newGPGKey(t *testing.T) gpgKey {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "GNUPGHOME="+home)

	params := filepath.Join(home, "params")
	must(t, os.WriteFile(params, []byte(
		"%no-protection\nKey-Type: RSA\nKey-Length: 3072\n"+
			"Name-Real: CR E2E\nName-Email: cr-e2e@example.com\nExpire-Date: 0\n%commit\n"), 0o600))
	gpgRun(t, env, "--batch", "--gen-key", params)

	k := gpgKey{home: home}
	k.keyID = gpgKeyID(t, env)
	k.pubFile = filepath.Join(home, "pub.asc")
	k.secFile = filepath.Join(home, "sec.asc")
	gpgExport(t, env, k.pubFile, "--export", "--armor", k.keyID)
	gpgExport(t, env, k.secFile, "--export-secret-keys", "--armor", k.keyID)
	// Make the key's keyring the default for this test, so both the CLI
	// subprocess and any in-process signing (pkg/sign) find it.
	t.Setenv("GNUPGHOME", home)
	return k
}

func gpgRun(t *testing.T, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("gpg", args...)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gpg %v: %v\n%s", args, err, out)
	}
}

func gpgExport(t *testing.T, env []string, outFile string, args ...string) {
	t.Helper()
	cmd := exec.Command("gpg", args...)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("gpg %v: %v", args, err)
	}
	must(t, os.WriteFile(outFile, out, 0o600))
}

func gpgKeyID(t *testing.T, env []string) string {
	t.Helper()
	cmd := exec.Command("gpg", "--list-keys", "--with-colons", "cr-e2e@example.com")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(line, ":")
		if len(f) > 9 && f[0] == "fpr" {
			return f[9]
		}
	}
	t.Fatal("no key fingerprint found")
	return ""
}

// gpgVerifyDetached checks an armored detached signature over data using key k.
func gpgVerifyDetached(t *testing.T, k gpgKey, sig, data []byte) error {
	t.Helper()
	dir := t.TempDir()
	sigFile := filepath.Join(dir, "sig.asc")
	dataFile := filepath.Join(dir, "data")
	must(t, os.WriteFile(sigFile, sig, 0o600))
	must(t, os.WriteFile(dataFile, data, 0o600))
	cmd := exec.Command("gpg", "--verify", sigFile, dataFile)
	cmd.Env = append(os.Environ(), "GNUPGHOME="+k.home)
	return cmd.Run()
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
