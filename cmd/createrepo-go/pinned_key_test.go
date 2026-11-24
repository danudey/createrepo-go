package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// gpgQuiet runs gpg in the isolated home without ever involving pinentry.
func gpgQuiet(t *testing.T, args ...string) string {
	t.Helper()
	full := append([]string{"--batch", "--pinentry-mode", "loopback", "--passphrase", ""}, args...)
	out, err := exec.Command("gpg", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("gpg %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// A key id pinned with "!" names one subkey. It must be recorded with the
// pin, so the next publish, which reads the key from the config, signs with
// that subkey and not the one gpg would choose (the newest).
func TestPinnedSubkeyIsRecordedAndReused(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not available")
	}
	isolateGPG(t)
	const uid = "pin test <pin@example.invalid>"
	gpgQuiet(t, "--quick-generate-key", uid, "rsa3072", "sign", "never")
	primary := fprs(t, uid)[0]
	gpgQuiet(t, "--quick-add-key", primary, "rsa2048", "sign", "never")
	gpgQuiet(t, "--quick-add-key", primary, "rsa2048", "sign", "never")
	all := fprs(t, uid)
	if len(all) != 3 {
		t.Fatalf("want a primary key and two subkeys, got %v", all)
	}
	pinned := all[1] // the older subkey; gpg would pick all[2]

	dir := t.TempDir()
	if out, err := runCLI(t, "add", dir, helloRPM, "--sign-metadata", "--gpg-key-id", pinned+"!"); err != nil {
		t.Fatalf("publish: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "createrepo-go.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		GPGKeyID string `json:"gpg_key_id"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.GPGKeyID != pinned+"!" {
		t.Fatalf("recorded gpg_key_id = %q, want %q", cfg.GPGKeyID, pinned+"!")
	}
	if got := metadataIssuer(t, dir); got != pinned[24:] {
		t.Errorf("first publish signed by %s, want the pinned subkey %s", got, pinned[24:])
	}

	// The second publish names no key: it comes from the config.
	if out, err := runCLI(t, "add", dir, libfooRPM); err != nil {
		t.Fatalf("second publish: %v\n%s", err, out)
	}
	if got := metadataIssuer(t, dir); got != pinned[24:] {
		t.Errorf("second publish signed by %s, want the pinned subkey %s", got, pinned[24:])
	}
}

// fprs lists the fingerprints of uid's key, primary first.
func fprs(t *testing.T, uid string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(gpgQuiet(t, "--with-colons", "--list-keys", uid), "\n") {
		if f := strings.Split(line, ":"); len(f) > 9 && f[0] == "fpr" {
			out = append(out, f[9])
		}
	}
	return out
}

// metadataIssuer returns the uppercase long key id that signed repomd.xml.
func metadataIssuer(t *testing.T, dir string) string {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "repodata", "repomd.xml.asc"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	block, err := armor.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	p, err := packet.Read(block.Body)
	if err != nil {
		t.Fatal(err)
	}
	sig, ok := p.(*packet.Signature)
	if !ok || sig.IssuerKeyId == nil {
		t.Fatalf("repomd.xml.asc holds %T, not an issued signature", p)
	}
	return fmt.Sprintf("%016X", *sig.IssuerKeyId)
}
