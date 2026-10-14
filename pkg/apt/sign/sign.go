// Package sign provides the signing and verification an apt repository needs:
// signing a suite's Release file, and verifying a repository's Release against
// a trusted key. The work itself is in package gpg, which createrepo-go shares;
// this package names it in apt's terms.
//
// Debian has no per-package signature that apt checks. A .deb is trusted
// because the signed Release vouches for the Packages index, which records the
// package's sha256 — so signing the Release is what makes every package in the
// repository trusted, and there is nothing to sign on a package itself.
package sign

import "github.com/danudey/createrepo-go/pkg/gpg"

// ReleaseSigner signs a suite's Release file in the two forms apt reads: a
// detached armored signature (Release.gpg) and an inline clearsigned document
// (InRelease). It implements repo.Signer.
type ReleaseSigner = gpg.Signer

// NewKeyFileSigner loads an OpenPGP private key from path (armored or binary),
// decrypting it with passphrase if it is protected.
func NewKeyFileSigner(path, passphrase string) (*ReleaseSigner, error) {
	return gpg.NewKeyFileSigner(path, passphrase)
}

// NewKeyIDSigner returns a signer that delegates to gpg, using the given key id
// (or any gpg-recognized user-id/fingerprint) from the local keyring.
func NewKeyIDSigner(keyID, passphrase string) *ReleaseSigner {
	return gpg.NewKeyIDSigner(keyID, passphrase)
}

// Fingerprint resolves a key file or keyring key id into the key's primary
// fingerprint; see gpg.Fingerprint.
func Fingerprint(keyFile, keyID string) (string, error) { return gpg.Fingerprint(keyFile, keyID) }

// VerifyDetached checks a Release.gpg over the Release it signs, returning the
// fingerprint of the key that made it.
func VerifyDetached(data, signature []byte, keyrings []string) (string, error) {
	return gpg.VerifyDetached(data, signature, keyrings)
}

// VerifyClearsigned checks an InRelease, returning the signed body and the
// fingerprint of the key that made the signature.
func VerifyClearsigned(document []byte, keyrings []string) (body []byte, fingerprint string, err error) {
	return gpg.VerifyClearsigned(document, keyrings)
}

// ExportPublicKeyToFile exports keyID into a temporary armored keyring file,
// returning its path and a cleanup function.
func ExportPublicKeyToFile(keyID string) (path string, cleanup func(), err error) {
	return gpg.ExportPublicKeyToFile(keyID)
}
