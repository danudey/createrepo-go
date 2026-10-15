package sign

import (
	"fmt"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/danudey/createrepo-go/pkg/gpg"
	"github.com/danudey/createrepo-go/pkg/rpm/pkgcheck"
)

// Verifier checks RPM package signatures against a set of trusted public keys.
//
// Verification is native (see pkgcheck), so it needs no rpm installation and
// gives the same answer on every host. Policy says which release's rules to
// apply: a package signed only with the rpm 6 OPENPGP tag is unsigned to the
// rpm on RHEL 8 and 9, whatever the rpm on the machine running this tool reads.
type Verifier struct {
	keyring openpgp.EntityList
	// Policy is the release whose rules signatures are judged by. The zero
	// value accepts every signature format.
	Policy pkgcheck.Policy
}

// NewVerifier builds a verifier trusting the public keys in the given armored
// or binary keyring files. At least one keyring is required.
func NewVerifier(keyringPaths ...string) (*Verifier, error) {
	if len(keyringPaths) == 0 {
		return nil, fmt.Errorf("--verify-sigs requires at least one --keyring")
	}
	kr, err := gpg.LoadKeyring(keyringPaths...)
	if err != nil {
		return nil, err
	}
	return &Verifier{keyring: kr}, nil
}

// Close releases the verifier. It holds nothing today; it is kept so callers
// need not change if verification ever needs resources again.
func (v *Verifier) Close() {}

// VerifyFile requires that the RPM at path be intact and carry at least one
// signature, readable on the policy's release, made by a trusted key. An
// unsigned package, a damaged one, a bad signature, or a signature from an
// unknown key (rpm's NOKEY) is rejected.
func (v *Verifier) VerifyFile(path string) error {
	res, err := pkgcheck.CheckFile(path, pkgcheck.Options{Keyring: v.keyring, Policy: v.Policy})
	if err != nil {
		return fmt.Errorf("verify %s: %w", path, err)
	}
	if !res.OK() {
		return fmt.Errorf("verify %s: bad signature or damaged package: %w", path, res.Err())
	}
	var untrusted, unreadable []string
	for _, s := range res.Signatures {
		switch {
		case s.Counts() && s.Status == pkgcheck.StatusGood:
			return nil
		case s.Counts():
			untrusted = append(untrusted, s.Signer())
		case !s.Readable:
			unreadable = append(unreadable, s.Tag)
		}
	}
	switch {
	case len(untrusted) > 0:
		return fmt.Errorf("verify %s: signed by an untrusted key (%s)", path, strings.Join(untrusted, ", "))
	case len(unreadable) > 0:
		return fmt.Errorf("verify %s: carries only an %s signature, which %s cannot read, so it installs there as unsigned; sign it with --signature-format v4",
			path, strings.Join(unreadable, "/"), policyName(v.Policy))
	}
	return fmt.Errorf("verify %s: no trusted signature (package unsigned)", path)
}

func policyName(p pkgcheck.Policy) string {
	if p.Target == "" {
		return "the target's rpm"
	}
	return p.Target
}
