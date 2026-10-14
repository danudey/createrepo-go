package sign

import "github.com/danudey/createrepo-go/pkg/gpg"

// Fingerprint resolves the primary-key fingerprint (uppercase hex, no spaces)
// of the signing key identified either by a private key file (keyFile) or by a
// key id/uid from the local GnuPG keyring (keyID). At most one should be set;
// if both are empty it returns ("", nil).
//
// The fingerprint is a portable, machine-independent identifier, so it is what
// gets recorded in the repository config regardless of how the key was supplied.
func Fingerprint(keyFile, keyID string) (string, error) { return gpg.Fingerprint(keyFile, keyID) }
