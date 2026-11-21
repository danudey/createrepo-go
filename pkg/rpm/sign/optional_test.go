package sign

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"

	"github.com/danudey/createrepo-go/internal/rpmtest"
)

// writePublicKey writes e's public key as an armored keyring file.
func writePublicKey(t *testing.T, e *openpgp.Entity) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	w.Close()
	p := filepath.Join(t.TempDir(), "pub.asc")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// An optional verifier passes unsigned packages and packages signed by keys it
// does not know, but still refuses a damaged package.
func TestOptionalVerification(t *testing.T) {
	base, err := os.ReadFile(referenceRPM)
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := rpmtest.NewKey("trusted")
	if err != nil {
		t.Fatal(err)
	}
	vendor, err := rpmtest.NewKey("vendor")
	if err != nil {
		t.Fatal(err)
	}
	vendorSigned, err := rpmtest.Signed(base, vendor, true, false)
	if err != nil {
		t.Fatal(err)
	}
	damaged := bytes.Clone(base)
	damaged[len(damaged)-10] ^= 0xff // in the payload

	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	unsigned, byVendor, broken := write("unsigned.rpm", base), write("vendor.rpm", vendorSigned), write("damaged.rpm", damaged)

	v, err := NewVerifier(writePublicKey(t, trusted))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{unsigned, byVendor} {
		if err := v.VerifyFile(p); err == nil {
			t.Errorf("a strict verifier accepted %s", filepath.Base(p))
		}
	}
	v.Optional = true
	for _, p := range []string{unsigned, byVendor} {
		if err := v.VerifyFile(p); err != nil {
			t.Errorf("an optional verifier refused %s: %v", filepath.Base(p), err)
		}
	}
	if err := v.VerifyFile(broken); err == nil {
		t.Error("an optional verifier accepted a damaged package")
	}
}
