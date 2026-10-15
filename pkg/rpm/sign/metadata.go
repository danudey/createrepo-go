// Package sign provides the three signing-related capabilities the repository
// tool needs: detached signing of repomd.xml, verification of RPM package
// signatures on add, and signing of RPM packages before upload.
//
// Metadata signing supports two key sources: a private key file (handled
// natively with OpenPGP) and a key identifier from the user's GnuPG keyring
// (delegated to the gpg binary). RPM verification (rpmkeys) and RPM package
// signing (rpmsign) delegate to the system rpm toolchain, because that is the
// robust, standard path and is always a local, pre-upload step.
package sign

import "github.com/danudey/createrepo-go/pkg/gpg"

// MetadataSigner produces an armored detached signature of repomd.xml. It
// implements repo.Signer. The work is done by package gpg, which createapt-go
// shares.
type MetadataSigner = gpg.Signer

// NewKeyFileSigner loads an OpenPGP private key from path (armored or binary)
// and returns a signer. If the key is passphrase-protected, passphrase is used
// to decrypt it.
func NewKeyFileSigner(path, passphrase string) (*MetadataSigner, error) {
	return gpg.NewKeyFileSigner(path, passphrase)
}

// NewKeyIDSigner returns a signer that delegates to gpg, using the given key id
// (or any gpg-recognized user-id/fingerprint) from the local keyring.
// passphrase, when set, is handed to gpg rather than prompted for.
func NewKeyIDSigner(keyID, passphrase string) *MetadataSigner {
	return gpg.NewKeyIDSigner(keyID, passphrase)
}
