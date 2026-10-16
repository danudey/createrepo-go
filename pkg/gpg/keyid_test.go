package gpg

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCheckKeyID(t *testing.T) {
	valid := []string{
		"ABCDEF01",
		"abcdef0123456789",
		"0xABCDEF0123456789",
		"ABCDEF0123456789ABCDEF0123456789ABCDEF01",
		"ABCDEF0123456789ABCDEF0123456789ABCDEF01!",
		"0xabcdef0123456789!",
		"ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789",
	}
	for _, s := range valid {
		if err := CheckKeyID(s); err != nil {
			t.Errorf("CheckKeyID(%q) = %v, want nil", s, err)
		}
	}
	invalid := []string{
		"",
		"!",
		"0x",
		"ABCDEF",                    // too short
		"ABCDEF0123456789ABCDEF012", // not a key id or fingerprint length
		"ABCDEF0123456789!!",
		"!ABCDEF0123456789",
		"ABCDEFGH",
		"CR Test <cr-test@example.com>",
		"--output=/tmp/pwned",
		"-ABCDEF0",
		"%(touch PWNED)",
		"ABCDEF01 %(id)",
		"ABCDEF01\n",
	}
	for _, s := range invalid {
		if err := CheckKeyID(s); err == nil {
			t.Errorf("CheckKeyID(%q) = nil, want an error", s)
		}
	}
}

func TestLongKeyID(t *testing.T) {
	for in, want := range map[string]string{
		"ABCDEF0123456789ABCDEF0123456789ABCDEF01":  "23456789abcdef01",
		"ABCDEF0123456789ABCDEF0123456789ABCDEF01!": "23456789abcdef01",
		"0xABCDEF0123456789":                        "abcdef0123456789",
		"0xABCDEF0123456789!":                       "abcdef0123456789",
		"ABCD EF01":                                 "abcdef01",
	} {
		if got := LongKeyID(in); got != want {
			t.Errorf("LongKeyID(%q) = %q, want %q", in, got, want)
		}
	}
}

// A key id that looks like a gpg option must be taken as a key id: before
// "--" was passed, gpg read "--output=<path>" as an option and wrote the
// exported keyring over that path.
func TestKeyIDIsNeverAnOption(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not available")
	}
	home := t.TempDir()
	t.Setenv("GNUPGHOME", home)
	// gpg writes nothing for an empty keyring, so there must be a key for the
	// injected --output to have something to write.
	gen := exec.Command("gpg", "--batch", "--passphrase", "", "--quick-generate-key",
		"CR Test <cr-test@example.com>", "ed25519", "sign", "never")
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generate key: %v: %s", err, out)
	}
	target := filepath.Join(t.TempDir(), "pwned")
	keyID := "--output=" + target

	if _, err := ExportPublicKey(keyID); err == nil {
		t.Error("ExportPublicKey of an option-shaped key id succeeded")
	}
	if _, err := Fingerprint("", keyID); err == nil {
		t.Error("Fingerprint of an option-shaped key id succeeded")
	}
	if _, err := os.Stat(target); err == nil {
		t.Errorf("gpg treated the key id as an option and wrote %s", target)
	}
}
