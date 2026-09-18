package sign

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Verifier checks RPM package signatures against a set of trusted public keys.
//
// Verification delegates to the system rpmkeys with an isolated temporary rpm
// keyring (a private --dbpath, so the host rpm database is never touched and no
// privileges are required). This is the robust path: rpmkeys understands every
// signature and payload-digest variant current rpm produces, which pure-Go
// libraries do not yet fully cover.
type Verifier struct {
	dbpath  string
	cleanup func()
}

// NewVerifier builds a verifier trusting the public keys in the given armored
// or binary keyring files. At least one keyring is required.
func NewVerifier(keyringPaths ...string) (*Verifier, error) {
	if len(keyringPaths) == 0 {
		return nil, fmt.Errorf("--verify-sigs requires at least one --keyring")
	}
	db, err := os.MkdirTemp("", "cr-rpmdb-*")
	if err != nil {
		return nil, err
	}
	cleanup := func() { os.RemoveAll(db) }
	for _, p := range keyringPaths {
		cmd := exec.Command("rpmkeys", "--dbpath", db, "--import", p)
		if out, err := cmd.CombinedOutput(); err != nil {
			cleanup()
			return nil, fmt.Errorf("import keyring %s: %w: %s", p, err, out)
		}
	}
	return &Verifier{dbpath: db, cleanup: cleanup}, nil
}

// Close removes the temporary keyring.
func (v *Verifier) Close() {
	if v.cleanup != nil {
		v.cleanup()
	}
}

// VerifyFile requires that the RPM at path carry at least one signature made by
// a trusted key. An unsigned package, a bad signature, or a signature from an
// unknown key (NOKEY) is rejected.
func (v *Verifier) VerifyFile(path string) error {
	cmd := exec.Command("rpmkeys", "--dbpath", v.dbpath, "-Kv", path)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()

	good := false
	sc := bufio.NewScanner(&buf)
	for sc.Scan() {
		line := strings.ToLower(strings.TrimSpace(sc.Text()))
		if !strings.Contains(line, "signature") {
			continue
		}
		switch {
		case strings.Contains(line, "nokey"):
			return fmt.Errorf("verify %s: signed by an untrusted key (%s)", path, strings.TrimSpace(sc.Text()))
		case strings.Contains(line, "bad") || strings.Contains(line, "not ok"):
			return fmt.Errorf("verify %s: bad signature (%s)", path, strings.TrimSpace(sc.Text()))
		case strings.HasSuffix(line, ": ok") || strings.HasSuffix(line, " ok"):
			good = true
		}
	}
	if good {
		return nil
	}
	if runErr != nil {
		return fmt.Errorf("verify %s: %w: %s", path, runErr, strings.TrimSpace(buf.String()))
	}
	return fmt.Errorf("verify %s: no trusted signature (package unsigned or signed by an unknown key)", path)
}
