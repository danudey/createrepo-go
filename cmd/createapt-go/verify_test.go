package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// testKey writes a throwaway signing key as an armored private key file and
// an armored public keyring, returning both paths.
func testKey(t *testing.T) (privatePath, publicPath string) {
	t.Helper()
	entity, err := openpgp.NewEntity("createapt test", "", "test@example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	privatePath = filepath.Join(dir, "signing.asc")
	publicPath = filepath.Join(dir, "public.asc")
	write := func(path, blockType string, fn func(w io.Writer) error) {
		var buf bytes.Buffer
		w, err := armor.Encode(&buf, blockType, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := fn(w); err != nil {
			t.Fatal(err)
		}
		w.Close()
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(privatePath, openpgp.PrivateKeyType, func(w io.Writer) error { return entity.SerializePrivate(w, nil) })
	write(publicPath, openpgp.PublicKeyType, func(w io.Writer) error { return entity.Serialize(w) })
	return privatePath, publicPath
}

func runApt(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := rootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

// signedSuite creates an empty suite signed with a new key, returning the
// repository directory and the public keyring that verifies it.
func signedSuite(t *testing.T) (dir, public string) {
	t.Helper()
	private, public := testKey(t)
	dir = t.TempDir()
	if out, err := runApt(t, "create", dir, "--gpg-key", private); err != nil {
		t.Fatalf("create: %v\n%s", err, out)
	}
	return dir, public
}

// verify checks the Release signature, which was accepted on the command line
// but never consulted: a repository with a forged signature verified OK.
func TestVerifyChecksTheReleaseSignature(t *testing.T) {
	dir, public := signedSuite(t)
	suiteDir := filepath.Join(dir, "dists", "stable")

	out, err := runApt(t, "verify", dir, "--keyring", public)
	if err != nil {
		t.Fatalf("verify of a correctly signed suite: %v\n%s", err, out)
	}
	for _, want := range []string{"verified: InRelease signed by", "verified: Release signed by"} {
		if !strings.Contains(out, want) {
			t.Errorf("verify output lacks %q:\n%s", want, out)
		}
	}

	_, other := testKey(t)
	if out, err := runApt(t, "verify", dir, "--keyring", other); err == nil {
		t.Errorf("verify with the wrong key passed:\n%s", out)
	}

	// Release changed after signing: InRelease still verifies, but clients
	// that read Release and Release.gpg get a document nobody signed.
	release := filepath.Join(suiteDir, "Release")
	data, err := os.ReadFile(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, append(data, []byte("X-Injected: 1\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = runApt(t, "verify", dir, "--keyring", public)
	if err == nil || !strings.Contains(out, "InRelease and Release differ") {
		t.Errorf("verify of a Release that no longer matches InRelease = %v:\n%s", err, out)
	}
}

// --verify-sigs makes an unsigned suite, or one with no key to check it
// against, a failure rather than a warning.
func TestVerifySigsRequiresASignature(t *testing.T) {
	dir := t.TempDir()
	if out, err := runApt(t, "create", dir, "--no-sign-release"); err != nil {
		t.Fatalf("create: %v\n%s", err, out)
	}
	_, public := testKey(t)
	if out, err := runApt(t, "verify", dir, "--keyring", public); err != nil {
		t.Errorf("verify of an unsigned suite without --verify-sigs should warn, not fail: %v\n%s", err, out)
	}
	if out, err := runApt(t, "verify", dir, "--keyring", public, "--verify-sigs"); err == nil {
		t.Errorf("verify --verify-sigs of an unsigned suite passed:\n%s", out)
	}
}
