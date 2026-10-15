package sign

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// awkwardPassphrase has everything that broke when the passphrase was pasted
// into an rpm macro: spaces, quotes, and rpm's own macro syntax, including the
// %(...) form rpm would run as a shell command.
const awkwardPassphrase = `two words "quoted" 'single' %{_tmppath} %(touch PWNED)`

// setupProtectedKey is setupKey for a key that needs awkwardPassphrase.
func setupProtectedKey(t *testing.T) (gnupgHome, keyID, pubFile string) {
	t.Helper()
	for _, tool := range []string{"gpg", "rpmsign", "rpmkeys"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	home := t.TempDir()
	os.Chmod(home, 0o700)
	env := append(os.Environ(), "GNUPGHOME="+home)
	passFile := filepath.Join(home, "pass")
	os.WriteFile(passFile, []byte(awkwardPassphrase), 0o600)
	run(t, env, "gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase-file", passFile,
		"--quick-generate-key", "CR Test <cr-test@example.com>", "rsa3072", "sign", "never")
	keyID = grabKeyID(t, env)
	pubFile = filepath.Join(home, "pub.asc")
	writeCmd(t, env, pubFile, "gpg", "--export", "--armor", keyID)
	return home, keyID, pubFile
}

func TestPackageSignWithAPassphrase(t *testing.T) {
	home, keyID, pubFile := setupProtectedKey(t)
	t.Setenv("GNUPGHOME", home)
	rpmFile := abs(t, referenceRPM)
	// Run where a stray file would be noticed: rpm runs %(...) in the
	// current directory.
	work := t.TempDir()
	t.Chdir(work)

	// A wrong passphrase must fail: it proves the passphrase is what unlocks
	// the key, rather than an agent having cached it.
	if _, _, err := NewPackageSignerKeyID(keyID, "wrong").SignFile(rpmFile); err == nil {
		t.Fatal("signing with the wrong passphrase succeeded")
	}

	signed, cleanup, err := NewPackageSignerKeyID(keyID, awkwardPassphrase).SignFile(rpmFile)
	if err != nil {
		t.Fatalf("sign with passphrase: %v", err)
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
	if _, err := os.Stat(filepath.Join(work, "PWNED")); err == nil {
		t.Error("rpm expanded the passphrase as a macro and ran a command from it")
	}
}

// The passphrase must never appear on a command line, where any local user can
// read it with ps, and the file that carries it must not outlive the signing.
func TestPackageSignKeepsThePassphraseOffTheCommandLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The stand-in rpmsign is a shell script, which Windows cannot run
		// (and rpm itself does not exist there).
		t.Skip("the fake rpmsign is a shell script")
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "args")
	script := `#!/bin/sh
printf '%s\n' "$@" >> "` + log + `"
for a in "$@"; do
  f=$(printf '%s' "$a" | sed -n "s/.*--passphrase-file '\([^']*\)'.*/\1/p")
  if [ -n "$f" ]; then
    echo "file=$f" >> "` + log + `"
    ls -l "$f" | cut -c1-10 >> "` + log + `"
    cat "$f" >> "` + log + `.pass"
  fi
done
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "rpmsign"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, cleanup, err := NewPackageSignerKeyID("ABCDEF", awkwardPassphrase).SignFile(abs(t, referenceRPM))
	if err != nil {
		t.Fatal(err)
	}
	cleanup()

	args, _ := os.ReadFile(log)
	if strings.Contains(string(args), "two words") {
		t.Errorf("the passphrase was passed on rpmsign's command line:\n%s", args)
	}
	pass, _ := os.ReadFile(log + ".pass")
	if string(pass) != awkwardPassphrase {
		t.Errorf("passphrase file held %q, want %q", pass, awkwardPassphrase)
	}
	if !strings.Contains(string(args), "-rw-------") {
		t.Errorf("passphrase file is not private (0600):\n%s", args)
	}
	for _, line := range strings.Split(string(args), "\n") {
		if f, ok := strings.CutPrefix(line, "file="); ok {
			if _, err := os.Stat(f); !os.IsNotExist(err) {
				t.Errorf("passphrase file %s outlived the signing (stat: %v)", f, err)
			}
		}
	}
}

func abs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
