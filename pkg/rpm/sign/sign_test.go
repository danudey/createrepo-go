package sign

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danudey/createrepo-go/pkg/rpm/pkgcheck"
)

// referenceRPM is an unsigned RPM produced by the repo test setup.
const referenceRPM = "../../../reference/rpm/rpmbuild/RPMS/noarch/hello-2.10-3.noarch.rpm"

// setupKey creates an ephemeral GnuPG home with a fresh signing key and exports
// public/secret key files. It skips the test if the required tools are absent.
func setupKey(t *testing.T) (gnupgHome, keyID, pubFile, secFile string) {
	t.Helper()
	for _, tool := range []string{"gpg", "rpmsign", "rpmkeys"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	home := t.TempDir()
	os.Chmod(home, 0o700)
	env := append(os.Environ(), "GNUPGHOME="+home)

	params := filepath.Join(home, "params")
	os.WriteFile(params, []byte("%no-protection\nKey-Type: RSA\nKey-Length: 3072\nName-Real: CR Test\nName-Email: cr-test@example.com\nExpire-Date: 0\n%commit\n"), 0o600)
	run(t, env, "gpg", "--batch", "--gen-key", params)

	pubFile = filepath.Join(home, "pub.asc")
	secFile = filepath.Join(home, "sec.asc")
	keyID = grabKeyID(t, env)
	writeCmd(t, env, pubFile, "gpg", "--export", "--armor", keyID)
	writeCmd(t, env, secFile, "gpg", "--export-secret-keys", "--armor", keyID)
	return home, keyID, pubFile, secFile
}

func TestPackageSignAndVerifyRoundTrip(t *testing.T) {
	home, keyID, pubFile, _ := setupKey(t)
	t.Setenv("GNUPGHOME", home)

	signer := NewPackageSignerKeyID(keyID, "")
	signed, cleanup, err := signer.SignFile(referenceRPM)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	v, err := NewVerifier(pubFile)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	if err := v.VerifyFile(signed); err != nil {
		t.Errorf("signed package should verify: %v", err)
	}
	if err := v.VerifyFile(referenceRPM); err == nil {
		t.Errorf("unsigned package should be rejected")
	}
}

func TestMetadataSignKeyFileAndKeyID(t *testing.T) {
	home, keyID, _, secFile := setupKey(t)
	t.Setenv("GNUPGHOME", home)
	data := []byte("<repomd>example</repomd>")

	fileSigner, err := NewKeyFileSigner(secFile, "")
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*MetadataSigner{
		"keyfile": fileSigner,
		"keyid":   NewKeyIDSigner(keyID, ""),
	} {
		sig, err := s.SignDetached(data)
		if err != nil {
			t.Fatalf("%s: sign: %v", name, err)
		}
		// Verify with gpg.
		sigFile := filepath.Join(t.TempDir(), "sig.asc")
		dataFile := filepath.Join(t.TempDir(), "data")
		os.WriteFile(sigFile, sig, 0o600)
		os.WriteFile(dataFile, data, 0o600)
		cmd := exec.Command("gpg", "--verify", sigFile, dataFile)
		cmd.Env = append(os.Environ(), "GNUPGHOME="+home)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: gpg verify failed: %v: %s", name, err, out)
		}
	}
}

func run(t *testing.T, env []string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

func writeCmd(t *testing.T, env []string, outFile, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	if err := os.WriteFile(outFile, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func grabKeyID(t *testing.T, env []string) string {
	t.Helper()
	cmd := exec.Command("gpg", "--list-keys", "--with-colons", "cr-test@example.com")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range splitLines(string(out)) {
		fields := splitColon(line)
		if len(fields) > 9 && fields[0] == "fpr" {
			return fields[9]
		}
	}
	t.Fatal("no key fingerprint found")
	return ""
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func splitColon(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func TestVerifierJudgesByTheTargetsRPM(t *testing.T) {
	home, keyID, pubFile, _ := setupKey(t)
	t.Setenv("GNUPGHOME", home)

	signer := NewPackageSignerKeyID(keyID, "")
	signer.Format = SigOpenPGP
	signed, cleanup, err := signer.SignFile(referenceRPM)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if sig, err := InspectPackage(signed); err != nil || sig.HeaderV4 {
		t.Skipf("this rpmsign writes a v4 header signature even for the openpgp format (err %v)", err)
	}

	v, err := NewVerifier(pubFile)
	if err != nil {
		t.Fatal(err)
	}
	for target, ok := range map[string]bool{pkgcheck.TargetRHEL8: false, pkgcheck.TargetRHEL9: false, pkgcheck.TargetRHEL10: true} {
		v.Policy, _ = pkgcheck.PolicyFor(target)
		err := v.VerifyFile(signed)
		if (err == nil) != ok {
			t.Errorf("%s: VerifyFile = %v, want ok=%v", target, err, ok)
		}
		if err != nil && !strings.Contains(err.Error(), "cannot read") {
			t.Errorf("%s: %v; want it to say the target cannot read the signature", target, err)
		}
	}
}

func TestDamagedPackagesAreNeverSigned(t *testing.T) {
	home, keyID, _, _ := setupKey(t)
	t.Setenv("GNUPGHOME", home)

	data, err := os.ReadFile(referenceRPM)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-10] ^= 0xff // the payload, which rpmsign does not look at
	damaged := filepath.Join(t.TempDir(), "hello.rpm")
	if err := os.WriteFile(damaged, data, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, replace := range []bool{false, true} {
		signer := NewPackageSignerKeyID(keyID, "")
		signer.Replace = replace
		if _, _, err := signer.SignFile(damaged); err == nil || !strings.Contains(err.Error(), "refusing to sign") {
			t.Errorf("replace=%v: SignFile = %v; want a refusal", replace, err)
		}
	}
}
