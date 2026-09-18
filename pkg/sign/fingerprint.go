package sign

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Fingerprint resolves the primary-key fingerprint (uppercase hex, no spaces)
// of the signing key identified either by a private key file (keyFile) or by a
// key id/uid from the local GnuPG keyring (keyID). At most one should be set;
// if both are empty it returns ("", nil).
//
// The fingerprint is a portable, machine-independent identifier, so it is what
// gets recorded in the repository config regardless of how the key was supplied.
func Fingerprint(keyFile, keyID string) (string, error) {
	switch {
	case keyFile != "":
		return keyFileFingerprint(keyFile)
	case keyID != "":
		return keyringFingerprint(keyID)
	default:
		return "", nil
	}
}

// keyFileFingerprint reads an OpenPGP key file (armored or binary) and returns
// its primary key's fingerprint.
func keyFileFingerprint(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	entities, err := readKeyRing(f)
	if err != nil {
		return "", fmt.Errorf("read key %s: %w", path, err)
	}
	for _, e := range entities {
		if e.PrimaryKey != nil {
			return fmt.Sprintf("%X", e.PrimaryKey.Fingerprint), nil
		}
	}
	return "", fmt.Errorf("no key found in %s", path)
}

// keyringFingerprint asks gpg for the fingerprint of a keyring key. keyID may be
// any identifier gpg accepts (short/long id, fingerprint, or user id).
func keyringFingerprint(keyID string) (string, error) {
	cmd := exec.Command(gpgBinary(), "--batch", "--with-colons", "--list-keys", keyID)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gpg list-keys %s: %w: %s", keyID, err, strings.TrimSpace(errBuf.String()))
	}
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) > 9 && fields[0] == "fpr" {
			return fields[9], nil
		}
	}
	return "", fmt.Errorf("no fingerprint found for key %q", keyID)
}
