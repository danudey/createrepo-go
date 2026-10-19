package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"

	"github.com/danudey/createrepo-go/pkg/gpg"
	"github.com/danudey/createrepo-go/pkg/rpm/repoconfig"
)

// subkeySigningKey writes a private key with a signing subkey, as production
// keys usually have, and returns its path and fingerprint.
func subkeySigningKey(t *testing.T) (path, fingerprint string) {
	t.Helper()
	e, err := openpgp.NewEntity("takeover test", "", "takeover@example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddSigningSubkey(nil); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SerializePrivate(w, nil); err != nil {
		t.Fatal(err)
	}
	w.Close()
	path = filepath.Join(t.TempDir(), "signing.asc")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, gpg.FingerprintOf(e)
}

// takeover --gpg-key <file> must recognise the repository's current signature
// as its own key's, and record the key's fingerprint. Before, the file path
// was used as the key id: every signature looked like another key's, which
// blocked the takeover, and adopting recorded the path as gpg_key_id.
func TestTakeoverWithAKeyFile(t *testing.T) {
	keyFile, fpr := subkeySigningKey(t)
	dir := t.TempDir()
	unmanaged(t, dir)
	// Sign repomd.xml with the key, as its current publisher did.
	signer, err := gpg.NewKeyFileSigner(keyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	repomd, err := os.ReadFile(filepath.Join(dir, "repodata", "repomd.xml"))
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signer.SignDetached(repomd)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repodata", "repomd.xml.asc"), sig, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, "takeover", dir, "--sign-metadata", "--gpg-key", keyFile, "--json")
	if err != nil {
		t.Fatalf("takeover with the key that already signs it: %v\n%s", err, out)
	}
	if !bytes.Contains([]byte(out), []byte(`"signed_metadata": true`)) {
		t.Fatalf("fixture: the repository is not signed:\n%s", out)
	}
	var rep struct {
		Findings []struct {
			Code string `json:"code"`
		} `json:"findings"`
		Config repoconfig.Config `json:"proposed_config"`
	}
	if err := json.NewDecoder(io.LimitReader(bytes.NewReader([]byte(out)), 1<<20)).Decode(&rep); err != nil {
		t.Fatalf("parse report: %v\n%s", err, out)
	}
	for _, f := range rep.Findings {
		if f.Code == "metadata-signer-changed" {
			t.Errorf("the signature by the key file's own subkey was reported as another key's:\n%s", out)
		}
	}
	if rep.Config.GPGKeyID != fpr {
		t.Errorf("proposed gpg_key_id = %q, want the key's fingerprint %s", rep.Config.GPGKeyID, fpr)
	}
}
