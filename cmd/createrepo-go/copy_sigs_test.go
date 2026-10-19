package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// metadataKeyPair writes a signing key as an armored private key file and a
// public keyring, returning both paths.
func metadataKeyPair(t *testing.T) (private, public string) {
	t.Helper()
	e, err := openpgp.NewEntity("copy test", "", "copy@example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, blockType string, ser func(*bytes.Buffer) error) string {
		var buf bytes.Buffer
		w, err := armor.Encode(&buf, blockType, nil)
		if err != nil {
			t.Fatal(err)
		}
		var raw bytes.Buffer
		if err := ser(&raw); err != nil {
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
	public = write("public.asc", openpgp.PublicKeyType, func(b *bytes.Buffer) error { return e.Serialize(b) })
	return private, public
}

// A source that signs its metadata but not its packages can be copied with
// its key at hand. Before, having a key made every unsigned package an error,
// so such a copy needed --skip-verify, which also dropped the metadata check.
func TestCopyOfAMetadataOnlySignedSource(t *testing.T) {
	private, public := metadataKeyPair(t)
	src := t.TempDir()
	if out, err := runCLI(t, "add", src, helloRPM, "--sign-metadata", "--gpg-key", private); err != nil {
		t.Fatalf("publish the source: %v\n%s", err, out)
	}

	if out, err := runCLI(t, "copy", src, t.TempDir(), "--keyring", public); err != nil {
		t.Errorf("copy of a metadata-only signed source with its key: %v\n%s", err, out)
	}
	if out, err := runCLI(t, "copy", src, t.TempDir(), "--keyring", public, "--verify-sigs"); err == nil {
		t.Errorf("copy --verify-sigs accepted unsigned packages:\n%s", out)
	}
}
