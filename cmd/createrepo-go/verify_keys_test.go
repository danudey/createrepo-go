package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// subkeyKeyFiles writes a key that signs with a signing subkey: its private
// key, its full public key, and a stale public key exported as if before the
// subkey was added.
func subkeyKeyFiles(t *testing.T) (private, full, stale string) {
	t.Helper()
	e, err := openpgp.NewEntity("verify test", "", "verify@example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	staleEntity := *e
	staleEntity.Subkeys = append([]openpgp.Subkey(nil), e.Subkeys...)
	if err := e.AddSigningSubkey(nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, blockType string, ser func(w *bytes.Buffer) error) string {
		var raw, buf bytes.Buffer
		if err := ser(&raw); err != nil {
			t.Fatal(err)
		}
		w, err := armor.Encode(&buf, blockType, nil)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(raw.Bytes())
		w.Close()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	private = write("signing.asc", openpgp.PrivateKeyType, func(b *bytes.Buffer) error { return e.SerializePrivate(b, nil) })
	full = write("full.asc", openpgp.PublicKeyType, func(b *bytes.Buffer) error { return e.Serialize(b) })
	stale = write("stale.asc", openpgp.PublicKeyType, func(b *bytes.Buffer) error { return staleEntity.Serialize(b) })
	return private, full, stale
}

// signedRepo publishes the hello RPM at dir with signed metadata.
func signedRepo(t *testing.T, dir, private string, extra ...string) {
	t.Helper()
	args := append([]string{"add", dir, helloRPM, "--sign-metadata", "--gpg-key", private}, extra...)
	if out, err := runCLI(t, args...); err != nil {
		t.Fatalf("publish: %v\n%s", err, out)
	}
}

// isolateGPG points gpg at an empty home, so the developer's keyring is not
// consulted for the repository's recorded key.
func isolateGPG(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	os.Chmod(home, 0o700)
	t.Setenv("GNUPGHOME", home)
	return home
}

func TestVerifyKeyringURL(t *testing.T) {
	isolateGPG(t)
	private, full, stale := subkeyKeyFiles(t)
	repo := t.TempDir()
	signedRepo(t, repo, private)

	out, err := runCLI(t, "verify", repo, "--keyring-url", "file://"+full)
	if err != nil {
		t.Fatalf("verify against the full public key: %v\n%s", err, out)
	}
	if !strings.Contains(out, "trusted keys: file://"+full) {
		t.Errorf("output does not name the trusted key:\n%s", out)
	}

	out, err = runCLI(t, "verify", repo, "--keyring-url", "file://"+stale)
	if err == nil || !strings.Contains(out, "the repomd.xml signature is by key") || !strings.Contains(out, "not a trusted key") {
		t.Errorf("verify against a public key missing the signing subkey: %v\n%s", err, out)
	}
	if strings.Contains(out, "not the repository's signing key\n") {
		t.Errorf("a subkey signature was called another key's when only the published copy was available:\n%s", out)
	}

	if _, err := runCLI(t, "verify", repo, "--keyring-url", "file:///nonexistent/key.asc"); err == nil {
		t.Error("verify accepted a --keyring-url that cannot be loaded")
	}
}

// With the signing key in the local keyring, verify knows the signature is
// one of the repository's subkeys, and says the published copy lacks it.
func TestVerifyExplainsAPublishedKeyMissingTheSubkey(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not available")
	}
	home := isolateGPG(t)
	private, full, stale := subkeyKeyFiles(t)
	if out, err := exec.Command("gpg", "--homedir", home, "--batch", "--import", full).CombinedOutput(); err != nil {
		t.Fatalf("import: %v\n%s", err, out)
	}
	repo := t.TempDir()
	signedRepo(t, repo, private)

	out, err := runCLI(t, "verify", repo, "--keyring-url", "file://"+stale)
	if err == nil || !strings.Contains(out, "lacks that subkey") {
		t.Errorf("verify did not explain the missing subkey: %v\n%s", err, out)
	}
}

// The gpg_key_url a publish records is the key verify checks against.
func TestVerifyUsesTheRecordedKeyURL(t *testing.T) {
	isolateGPG(t)
	private, full, stale := subkeyKeyFiles(t)
	serve := full
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		data, _ := os.ReadFile(serve)
		w.Write(data)
	}))
	defer srv.Close()

	repo := t.TempDir()
	signedRepo(t, repo, private, "--gpg-key-url", srv.URL+"/key.asc")
	out, err := runCLI(t, "verify", repo)
	if err != nil || !strings.Contains(out, "not trusted") {
		t.Fatalf("verify with the recorded key URL: %v\n%s", err, out)
	}
	if strings.Contains(out, "verified against a trusted key") {
		t.Errorf("the repository's own key URL was treated as trusted:\n%s", out)
	}

	serve = stale
	out, err = runCLI(t, "verify", repo)
	if err == nil || !strings.Contains(out, "does not contain") {
		t.Errorf("verify passed although the recorded key URL serves a key without the signing subkey: %v\n%s", err, out)
	}
}

// A repository cannot vouch for itself: a recorded gpg_key_url serving a key
// that really did sign it does not make its signatures trusted, so
// --require-trusted still needs an operator-supplied key.
func TestVerifyDoesNotTrustTheRepositorysOwnKeyURL(t *testing.T) {
	isolateGPG(t)
	private, full, _ := subkeyKeyFiles(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		data, _ := os.ReadFile(full)
		w.Write(data)
	}))
	defer srv.Close()
	repo := t.TempDir()
	signedRepo(t, repo, private, "--gpg-key-url", srv.URL+"/key.asc")

	if out, err := runCLI(t, "verify", repo, "--require-trusted"); err == nil {
		t.Errorf("--require-trusted passed on the strength of the repository's own key URL:\n%s", out)
	}
	if out, err := runCLI(t, "verify", repo, "--require-trusted", "--keyring", full); err != nil {
		t.Errorf("--require-trusted with an operator-supplied key: %v\n%s", err, out)
	}
}

func TestVerifyRepoFile(t *testing.T) {
	isolateGPG(t)
	private, full, stale := subkeyKeyFiles(t)
	root := t.TempDir()
	repo := filepath.Join(root, "9", "x86_64")
	signedRepo(t, repo, private)

	writeRepo := func(body string) string {
		p := filepath.Join(t.TempDir(), "example.repo")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	repoFile := func(gpgcheck, key string) string {
		return writeRepo("[example]\nname=Example\nbaseurl=file://" + root + "/$releasever/$basearch\n" +
			"gpgcheck=" + gpgcheck + "\nrepo_gpgcheck=1\ngpgkey=file://" + key + "\n\n" +
			"[disabled]\nbaseurl=https://example.invalid/$contentdir/\nenabled=0\n")
	}

	good := repoFile("0", full)
	out, err := runCLI(t, "verify", good)
	if err == nil || !strings.Contains(err.Error(), "$basearch, $releasever") || !strings.Contains(err.Error(), "--releasever") {
		t.Errorf("verify without variable values = %v, want an error naming them\n%s", err, out)
	}

	out, err = runCLI(t, "verify", good, "--releasever", "9", "--arch", "x86_64")
	if err != nil {
		t.Fatalf("verify a good .repo file: %v\n%s", err, out)
	}
	for _, want := range []string{"[disabled] skipped", "[example releasever=9 arch=x86_64] verifying file://" + root + "/9/x86_64", "repo_gpgcheck=1", "[example releasever=9 arch=x86_64] trusted keys: file://" + full} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	out, err = runCLI(t, "verify", repoFile("0", stale), "--releasever", "9", "--var", "basearch=x86_64")
	if err == nil || !strings.Contains(out, "[example releasever=9] PROBLEM:") {
		t.Errorf("repo_gpgcheck=1 with a key missing the signing subkey: %v\n%s", err, out)
	}

	// The hello RPM is unsigned, which gpgcheck=1 refuses.
	out, err = runCLI(t, "verify", repoFile("1", full), "--releasever", "9", "--arch", "x86_64")
	if err == nil || !strings.Contains(out, "it is not signed") {
		t.Errorf("gpgcheck=1 with unsigned packages: %v\n%s", err, out)
	}

	noKey := writeRepo("[example]\nbaseurl=file://" + repo + "\nrepo_gpgcheck=1\n")
	out, err = runCLI(t, "verify", noKey, "--keyring-url", "file://"+full)
	if err == nil || !strings.Contains(out, "there is no gpgkey=") {
		t.Errorf("repo_gpgcheck=1 without gpgkey=: %v\n%s", err, out)
	}
}
