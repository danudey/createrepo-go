package pkgcheck

import (
	"crypto"
	"fmt"

	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// Policy is what one release's rpm accepts from a package signature. A
// signature the policy rejects makes the package unverifiable on that release,
// however sound the signature is.
type Policy struct {
	// Target is the canonical release name ("rhel8", …), or "" for none.
	Target string
	// ReadsOpenPGPTag reports whether the release's rpm reads the rpm 6
	// OPENPGP signature tag. rpm 4.14 (RHEL 8) and 4.16 (RHEL 9) do not, and
	// treat a package signed only that way as unsigned.
	ReadsOpenPGPTag bool
	// RejectSHA1 rejects signatures made over a SHA-1 hash.
	RejectSHA1 bool
	// RejectDSA rejects DSA signatures.
	RejectDSA bool
	// MinRSABits is the smallest RSA key accepted; 0 means any.
	MinRSABits int
}

// Targets the policies below describe. They match the --target profiles.
const (
	TargetRHEL8  = "rhel8"
	TargetRHEL9  = "rhel9"
	TargetRHEL10 = "rhel10"
)

// PolicyFor returns the signature policy of a canonical target name. The empty
// target accepts every signature format and applies no crypto policy beyond
// refusing MD5, which nothing accepts.
//
// RHEL 9 and later reject SHA-1 signatures, DSA, and RSA keys under 2048 bits
// in their DEFAULT system-wide crypto policy; RHEL 8's DEFAULT policy does not
// constrain rpm signatures.
func PolicyFor(target string) (Policy, error) {
	switch target {
	case "":
		return Policy{ReadsOpenPGPTag: true}, nil
	case TargetRHEL8:
		return Policy{Target: target}, nil
	case TargetRHEL9:
		return Policy{Target: target, RejectSHA1: true, RejectDSA: true, MinRSABits: 2048}, nil
	case TargetRHEL10:
		return Policy{Target: target, ReadsOpenPGPTag: true, RejectSHA1: true, RejectDSA: true, MinRSABits: 2048}, nil
	}
	return Policy{}, fmt.Errorf("unknown target %q (valid: %s, %s, %s)", target, TargetRHEL8, TargetRHEL9, TargetRHEL10)
}

// name is how the policy's release is written in a report.
func (p Policy) name() string {
	if p.Target == "" {
		return "rpm"
	}
	return p.Target
}

// rejects reports why the policy refuses a signature made with hash h by a key
// of algorithm algo and bits size (0 when the key is not available), or "".
func (p Policy) rejects(h crypto.Hash, algo packet.PublicKeyAlgorithm, bits int) string {
	switch {
	case h == crypto.MD5:
		return "it is made over an MD5 hash, which no rpm accepts"
	case p.RejectSHA1 && h == crypto.SHA1:
		return fmt.Sprintf("it is made over a SHA-1 hash, which %s rejects", p.name())
	case p.RejectDSA && algo == packet.PubKeyAlgoDSA:
		return fmt.Sprintf("it is a DSA signature, which %s rejects", p.name())
	case p.MinRSABits > 0 && bits > 0 && bits < p.MinRSABits &&
		(algo == packet.PubKeyAlgoRSA || algo == packet.PubKeyAlgoRSASignOnly):
		return fmt.Sprintf("it is made by a %d-bit RSA key, and %s requires at least %d bits", bits, p.name(), p.MinRSABits)
	}
	return ""
}
