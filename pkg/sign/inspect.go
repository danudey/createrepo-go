package sign

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/cavaliergopher/rpm"
)

// rpm signature-header tag identifiers holding an OpenPGP signature. The rpm
// library exports no constants for them, so they are declared here (matching
// rpmtag.h). RSAHEADER/DSAHEADER sign the header only; PGP/GPG sign the header
// plus the payload.
const (
	tagSigDSAHeader = 267
	tagSigRSAHeader = 268
	tagSigPGP       = 1002
	tagSigGPG       = 1005
	tagSigPGP5      = 1006
)

// PackageKeyIDs reads the signature header of the RPM at path and reports the
// OpenPGP key ids (lowercase 16-hex) that signed it, along with whether the
// package carries a signature at all. The two answers are distinct: signed
// with no ids means the signature is in a format this build cannot read, which
// callers must treat as "unknown" rather than "unsigned".
func PackageKeyIDs(path string) (ids []string, signed bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	pkg, err := rpm.Read(f)
	if err != nil {
		return nil, false, fmt.Errorf("read rpm %s: %w", path, err)
	}

	seen := map[string]bool{}
	for _, tag := range []int{tagSigRSAHeader, tagSigDSAHeader, tagSigPGP, tagSigGPG, tagSigPGP5} {
		raw := pkg.Signature.GetTag(tag).Bytes()
		if len(raw) == 0 {
			continue
		}
		signed = true
		id, ok := signatureKeyID(raw)
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, signed, nil
}

// signatureKeyID extracts the issuer key id from a raw (binary) OpenPGP
// signature packet.
func signatureKeyID(raw []byte) (string, bool) {
	r := packet.NewReader(bytes.NewReader(raw))
	for {
		p, err := r.Next()
		if err != nil {
			return "", false
		}
		sig, ok := p.(*packet.Signature)
		if !ok {
			continue
		}
		if sig.IssuerKeyId != nil {
			return fmt.Sprintf("%016x", *sig.IssuerKeyId), true
		}
		// A v6 signature carries only the fingerprint; its trailing bytes are
		// the key id by construction.
		if n := len(sig.IssuerFingerprint); n >= 8 {
			return fmt.Sprintf("%x", sig.IssuerFingerprint[n-8:]), true
		}
	}
}

// SameSigner reports whether two sets of key ids overlap, i.e. whether the same
// key signed both packages. It returns false when either set is empty, so an
// undetermined signer never masquerades as a match.
func SameSigner(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	in := make(map[string]bool, len(a))
	for _, id := range a {
		in[id] = true
	}
	for _, id := range b {
		if in[id] {
			return true
		}
	}
	return false
}

// gpgRecordFpr is the record type of a fingerprint line in gpg's
// --with-colons output; field 10 holds the fingerprint itself.
const gpgRecordFpr = "fpr"

// KeyAndSubkeyIDs expands a key identifier into every long key id that can
// issue a signature on its behalf: the primary key's and each subkey's. A
// repository config records only the primary fingerprint, but gpg signs with a
// signing subkey when the key has one, so comparing a recorded fingerprint
// against an actual signature's issuer needs the whole set. When the key is not
// in the local keyring the identifier itself is all we can offer.
func KeyAndSubkeyIDs(keyID string) []string {
	self := longKeyID(keyID)
	cmd := exec.Command(gpgBinary(), "--batch", "--with-colons", "--list-keys", keyID)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return []string{self}
	}
	seen := map[string]bool{self: true}
	ids := []string{self}
	for _, line := range strings.Split(out.String(), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) > 9 && fields[0] == gpgRecordFpr {
			if id := longKeyID(fields[9]); id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	return ids
}

// longKeyID reduces a fingerprint to the lowercase 16-hex long key id that a
// signature packet carries, leaving anything shorter alone.
func longKeyID(s string) string {
	s = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	if len(s) > 16 {
		return s[len(s)-16:]
	}
	return s
}

// VerifyDetached checks an ASCII-armored detached signature over data against
// the public keys in the given keyring files. It returns the fingerprint of the
// key that made the signature.
func VerifyDetached(data, armoredSig []byte, keyringPaths []string) (string, error) {
	if len(keyringPaths) == 0 {
		return "", fmt.Errorf("no keyring supplied to verify the signature against")
	}
	var ring openpgp.EntityList
	for _, p := range keyringPaths {
		el, err := ReadKeyRingFile(p)
		if err != nil {
			return "", fmt.Errorf("read keyring %s: %w", p, err)
		}
		ring = append(ring, el...)
	}
	if len(ring) == 0 {
		return "", fmt.Errorf("no OpenPGP keys found in the supplied keyring(s)")
	}
	signer, err := openpgp.CheckArmoredDetachedSignature(ring, bytes.NewReader(data), bytes.NewReader(armoredSig), nil)
	if err != nil {
		return "", err
	}
	if signer == nil || signer.PrimaryKey == nil {
		return "", fmt.Errorf("signature verified but the signing key could not be identified")
	}
	return fmt.Sprintf("%X", signer.PrimaryKey.Fingerprint), nil
}

// ReadKeyRingFile reads OpenPGP entities from a key file, accepting either
// armored or binary input.
func ReadKeyRingFile(path string) (openpgp.EntityList, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if el, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(data)); err == nil {
		return el, nil
	}
	return openpgp.ReadKeyRing(bytes.NewReader(data))
}

// ExportPublicKey writes the public half of a key from the local GnuPG keyring
// to a temporary armored file, returning its path and a cleanup function. It is
// how a repository that records only a key fingerprint (in createrepo-go.json)
// gets turned into something rpmkeys and OpenPGP verification can use. A key
// that is not in the local keyring yields an error.
func ExportPublicKey(keyID string) (path string, cleanup func(), err error) {
	noop := func() {}
	if keyID == "" {
		return "", noop, fmt.Errorf("no key id given")
	}
	cmd := exec.Command(gpgBinary(), "--batch", "--armor", "--export", keyID)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", noop, fmt.Errorf("gpg --export %s: %w: %s", keyID, err, strings.TrimSpace(errBuf.String()))
	}
	if out.Len() == 0 {
		return "", noop, fmt.Errorf("key %s is not in the local GnuPG keyring", keyID)
	}
	f, err := os.CreateTemp("", "cr-pubkey-*.asc")
	if err != nil {
		return "", noop, err
	}
	if _, err := io.Copy(f, &out); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", noop, err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", noop, err
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}
