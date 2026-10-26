package sign

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeRPMSign puts a stand-in rpmsign first on PATH that logs its arguments,
// one per line, and succeeds. It returns the log's path.
func fakeRPMSign(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake rpmsign is a shell script")
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"" + log + "\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "rpmsign"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// A key id rpm would expand as a macro must never reach rpmsign: it is not a
// hexadecimal key id, so it is resolved through gpg first, and with no such key
// signing fails before rpmsign runs.
func TestPackageSignRefusesAMacroInTheKeyID(t *testing.T) {
	log := fakeRPMSign(t)
	t.Setenv("GNUPGHOME", t.TempDir())
	rpmFile := abs(t, referenceRPM)
	work := t.TempDir()
	t.Chdir(work)

	if _, _, err := NewPackageSignerKeyID("%(touch PWNED)", "").SignFile(rpmFile); err == nil {
		t.Fatal("signing with a macro for a key id succeeded")
	}
	if args, _ := os.ReadFile(log); len(args) > 0 {
		t.Errorf("rpmsign ran with an unresolved key id:\n%s", args)
	}
	if _, err := os.Stat(filepath.Join(work, "PWNED")); err == nil {
		t.Error("the key id was expanded as a macro")
	}
}

// A hexadecimal key id goes to rpm unchanged, including gpg's trailing "!"
// that pins signing to that exact key rather than one of its subkeys.
func TestPackageSignPassesAnExactKeyIDThrough(t *testing.T) {
	log := fakeRPMSign(t)
	const keyID = "ABCDEF0123456789ABCDEF0123456789ABCDEF01!"

	_, cleanup, err := NewPackageSignerKeyID(keyID, "").SignFile(abs(t, referenceRPM))
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	args, _ := os.ReadFile(log)
	if !strings.Contains(string(args), "_gpg_name "+keyID+"\n") {
		t.Errorf("rpmsign was not given _gpg_name %s:\n%s", keyID, args)
	}
}
