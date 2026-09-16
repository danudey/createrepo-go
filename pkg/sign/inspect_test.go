package sign

import (
	"strings"
	"testing"
)

// longID reduces a fingerprint to the lowercase long key id that a signature
// packet carries.
func longID(fingerprint string) string {
	s := strings.ToLower(fingerprint)
	if len(s) > 16 {
		return s[len(s)-16:]
	}
	return s
}

func TestPackageKeyIDsDistinguishesUnsignedFromSigned(t *testing.T) {
	home, keyID, _, _ := setupKey(t)
	t.Setenv("GNUPGHOME", home)

	ids, signed, err := PackageKeyIDs(referenceRPM)
	if err != nil {
		t.Fatal(err)
	}
	if signed || len(ids) != 0 {
		t.Errorf("the reference RPM is unsigned; got signed=%v ids=%v", signed, ids)
	}

	signer := NewPackageSignerKeyID(keyID, "")
	signedRPM, cleanup, err := signer.SignFile(referenceRPM)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	ids, signed, err = PackageKeyIDs(signedRPM)
	if err != nil {
		t.Fatal(err)
	}
	if !signed {
		t.Fatal("a freshly signed package should report as signed")
	}
	if len(ids) == 0 {
		t.Fatal("no issuer key id could be read from the signature")
	}
	want := longID(keyID)
	for _, id := range ids {
		if id != want {
			t.Errorf("signature issuer %s, want %s", id, want)
		}
	}
}

func TestSameSignerNeedsBothSidesKnown(t *testing.T) {
	if SameSigner(nil, []string{"aabbccddeeff0011"}) {
		t.Error("an unknown signer must never count as a match")
	}
	if SameSigner([]string{"aabbccddeeff0011"}, nil) {
		t.Error("an unknown signer must never count as a match")
	}
	if !SameSigner([]string{"aabbccddeeff0011"}, []string{"0011223344556677", "aabbccddeeff0011"}) {
		t.Error("overlapping key ids should match")
	}
	if SameSigner([]string{"aabbccddeeff0011"}, []string{"0011223344556677"}) {
		t.Error("different key ids should not match")
	}
}

func TestVerifyDetachedAndExportPublicKey(t *testing.T) {
	home, keyID, pubFile, _ := setupKey(t)
	t.Setenv("GNUPGHOME", home)

	data := []byte("<repomd>example</repomd>")
	sig, err := NewKeyIDSigner(keyID).SignDetached(data)
	if err != nil {
		t.Fatal(err)
	}

	fpr, err := VerifyDetached(data, sig, []string{pubFile})
	if err != nil {
		t.Fatalf("a signature made by this key should verify: %v", err)
	}
	if !strings.EqualFold(fpr, keyID) {
		t.Errorf("signed by %s, want %s", fpr, keyID)
	}

	if _, err := VerifyDetached([]byte("tampered"), sig, []string{pubFile}); err == nil {
		t.Error("a signature over different content must not verify")
	}

	// The public key can be recovered from the local keyring by fingerprint,
	// which is all a repository config records.
	exported, cleanup, err := ExportPublicKey(keyID)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := VerifyDetached(data, sig, []string{exported}); err != nil {
		t.Errorf("the exported key should verify the same signature: %v", err)
	}
	if _, _, err := ExportPublicKey("0000000000000000000000000000000000000000"); err == nil {
		t.Error("exporting a key that is not in the keyring should fail")
	}
}
