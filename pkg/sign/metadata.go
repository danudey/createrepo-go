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

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// MetadataSigner produces an armored detached signature of repomd.xml. It
// implements repo.Signer.
type MetadataSigner struct {
	// entity is set for key-file signing (native).
	entity *openpgp.Entity
	// keyID is set for keyring signing (delegated to gpg).
	keyID string
}

// NewKeyFileSigner loads an OpenPGP private key from path (armored or binary)
// and returns a signer. If the key is passphrase-protected, passphrase is used
// to decrypt it.
func NewKeyFileSigner(path, passphrase string) (*MetadataSigner, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	entities, err := readKeyRing(f)
	if err != nil {
		return nil, fmt.Errorf("read signing key %s: %w", path, err)
	}
	var signer *openpgp.Entity
	for _, e := range entities {
		if e.PrivateKey != nil {
			signer = e
			break
		}
	}
	if signer == nil {
		return nil, fmt.Errorf("no private key found in %s", path)
	}
	if signer.PrivateKey.Encrypted {
		if passphrase == "" {
			return nil, fmt.Errorf("signing key %s is encrypted but no passphrase was provided", path)
		}
		if err := decryptEntity(signer, []byte(passphrase)); err != nil {
			return nil, fmt.Errorf("decrypt signing key: %w", err)
		}
	}
	return &MetadataSigner{entity: signer}, nil
}

// NewKeyIDSigner returns a signer that delegates to gpg, using the given key id
// (or any gpg-recognized user-id/fingerprint) from the local keyring.
func NewKeyIDSigner(keyID string) *MetadataSigner {
	return &MetadataSigner{keyID: keyID}
}

// SignDetached returns an ASCII-armored detached signature over data.
func (s *MetadataSigner) SignDetached(data []byte) ([]byte, error) {
	if s.entity != nil {
		var buf bytes.Buffer
		if err := openpgp.ArmoredDetachSign(&buf, s.entity, bytes.NewReader(data), nil); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	return gpgDetachSign(s.keyID, data)
}

// gpgDetachSign shells out to gpg to produce an armored detached signature.
func gpgDetachSign(keyID string, data []byte) ([]byte, error) {
	args := []string{"--batch", "--yes", "--armor", "--detach-sign", "--output", "-"}
	if keyID != "" {
		args = append(args, "--local-user", keyID)
	}
	cmd := exec.Command(gpgBinary(), args...)
	cmd.Stdin = bytes.NewReader(data)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gpg sign: %v: %s", err, errBuf.String())
	}
	return out.Bytes(), nil
}

func gpgBinary() string {
	if g := os.Getenv("CREATEREPO_GPG"); g != "" {
		return g
	}
	return "gpg"
}

// readKeyRing reads OpenPGP entities, accepting either armored or binary input.
func readKeyRing(f *os.File) (openpgp.EntityList, error) {
	data, err := os.ReadFile(f.Name())
	if err != nil {
		return nil, err
	}
	if block, err := armor.Decode(bytes.NewReader(data)); err == nil {
		return openpgp.ReadKeyRing(block.Body)
	}
	return openpgp.ReadKeyRing(bytes.NewReader(data))
}

// decryptEntity decrypts a private key and its subkeys in place.
func decryptEntity(e *openpgp.Entity, passphrase []byte) error {
	if e.PrivateKey != nil && e.PrivateKey.Encrypted {
		if err := e.PrivateKey.Decrypt(passphrase); err != nil {
			return err
		}
	}
	for _, sk := range e.Subkeys {
		if sk.PrivateKey != nil && sk.PrivateKey.Encrypted {
			if err := sk.PrivateKey.Decrypt(passphrase); err != nil {
				return err
			}
		}
	}
	return nil
}
