package pkgcheck

import (
	"bytes"
	"crypto"
	"encoding"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// Status is the outcome of checking one signature.
type Status int

const (
	// StatusGood means the signature verifies against a trusted key.
	StatusGood Status = iota
	// StatusUntrusted means the signature is intact — it matches the data it signs
	// — but no trusted key made it, so who made it is unproven. rpm reports
	// this as NOKEY.
	StatusUntrusted
	// StatusBad means the signature does not match the data, does not verify
	// against the key that claims it, or cannot be parsed.
	StatusBad
	// StatusUnsupported means the signature is in a format this build cannot
	// check, such as an OpenPGP v3 packet.
	StatusUnsupported
	// StatusUnchecked means the signature covers the payload, which was not read.
	StatusUnchecked
)

func (s Status) String() string {
	switch s {
	case StatusGood:
		return "good"
	case StatusUntrusted:
		return "untrusted"
	case StatusBad:
		return "bad"
	case StatusUnsupported:
		return "unsupported"
	case StatusUnchecked:
		return "unchecked"
	}
	return fmt.Sprintf("Status(%d)", int(s))
}

// Signature describes one OpenPGP signature and what checking it found.
type Signature struct {
	// Tag names where the signature was found: an rpm signature tag
	// ("RSAHEADER", "OPENPGP", …) or "detached".
	Tag string
	// KeyID is the issuer's long key id, lowercase hex, when the signature
	// names one.
	KeyID string
	// Fingerprint is the primary-key fingerprint of the key that verified the
	// signature, uppercase hex. Set only for StatusGood.
	Fingerprint string
	// Algorithm and Hash name the signature's public-key and hash algorithms.
	Algorithm string
	Hash      string
	Status    Status
	// Detail explains a status other than good.
	Detail string
	// Readable reports whether the target's rpm reads the tag the signature
	// is in. A signature it cannot read does not make the package signed
	// there.
	Readable bool
	// Rejected, when set, says why the target's crypto policy refuses the
	// signature even though it verifies.
	Rejected string
}

// Signer identifies who made the signature: the verified key's fingerprint,
// or the issuer key id when no trusted key verified it.
func (s Signature) Signer() string {
	if s.Fingerprint != "" {
		return s.Fingerprint
	}
	return s.KeyID
}

// Counts reports whether the signature makes the package signed for the
// target: readable there, intact, and not refused by its policy.
func (s Signature) Counts() bool {
	return s.Readable && s.Rejected == "" && (s.Status == StatusGood || s.Status == StatusUntrusted)
}

func (s Signature) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s signature", s.Tag)
	if s.KeyID != "" {
		fmt.Fprintf(&b, " by key %s", s.Signer())
	}
	fmt.Fprintf(&b, ": %s", s.Status)
	if s.Detail != "" {
		fmt.Fprintf(&b, " (%s)", s.Detail)
	}
	return b.String()
}

// sigCheck is one signature being checked. The data it signs is written to it
// as it streams past, so a signature over a whole package costs no extra pass.
type sigCheck struct {
	out  Signature
	sig  *packet.Signature
	h    hash.Hash
	text bool // canonicalize line endings: a text-mode signature
	cr   bool // the previous byte written was '\r'
}

// newSigCheck parses one raw signature packet.
func newSigCheck(tag string, raw []byte) *sigCheck {
	p, err := packet.Read(bytes.NewReader(raw))
	if err != nil {
		c := &sigCheck{out: Signature{Tag: tag, Status: StatusBad, Detail: "malformed OpenPGP signature: " + err.Error()}}
		if _, ok := errors.AsType[pgperrors.UnsupportedError](err); ok {
			c.out.Status = StatusUnsupported
			c.out.Detail = "unsupported OpenPGP signature: " + err.Error()
		}
		return c
	}
	sig, ok := p.(*packet.Signature)
	if !ok {
		return &sigCheck{out: Signature{Tag: tag, Status: StatusBad, Detail: fmt.Sprintf("holds an OpenPGP %T packet, not a signature", p)}}
	}
	return newSigCheckPacket(tag, sig)
}

// newSigCheckPacket prepares a parsed signature for checking.
func newSigCheckPacket(tag string, sig *packet.Signature) *sigCheck {
	c := &sigCheck{sig: sig, out: Signature{
		Tag:       tag,
		KeyID:     issuerKeyID(sig),
		Algorithm: algorithmName(sig.PubKeyAlgo),
		Hash:      hashName(sig.Hash),
	}}
	h, err := sig.PrepareVerify()
	if err != nil {
		c.out.Status = StatusUnsupported
		c.out.Detail = err.Error()
		return c
	}
	c.h = h
	c.text = sig.SigType == packet.SigTypeText
	return c
}

// Write feeds signed data to the signature's hash. It never fails: it is an
// io.Writer so a signature can sit in an io.MultiWriter with the digests.
func (c *sigCheck) Write(p []byte) (int, error) {
	c.feed(p)
	return len(p), nil
}

// feed adds p to the signed data.
func (c *sigCheck) feed(p []byte) {
	if c.h == nil {
		return
	}
	if !c.text {
		c.h.Write(p)
		return
	}
	// A text signature is made over the document with CRLF line endings.
	for _, b := range p {
		if b == '\n' && !c.cr {
			c.h.Write([]byte{'\r'})
		}
		c.h.Write([]byte{b})
		c.cr = b == '\r'
	}
}

// finish completes the check once all the signed data has been written.
func (c *sigCheck) finish(keyring openpgp.EntityList, pol Policy, now time.Time) Signature {
	out := c.out
	if c.h == nil {
		return out
	}

	// The two leading bytes of the hash are stored in the signature, so a
	// signature over different data — damaged, or copied from another
	// package — is caught without any key.
	h, err := cloneHash(c.h, c.sig.Hash)
	if err != nil {
		out.Status, out.Detail = StatusUnsupported, err.Error()
		return out
	}
	h.Write(c.sig.HashSuffix)
	if sum := h.Sum(nil); len(sum) < 2 || sum[0] != c.sig.HashTag[0] || sum[1] != c.sig.HashTag[1] {
		out.Status, out.Detail = StatusBad, "it does not match the data it signs"
		return out
	}

	bits := 0
	out.Status = StatusUntrusted
	if id, ok := issuerID(c.sig); ok && len(keyring) > 0 {
		keys := keyring.KeysById(id)
		var lastErr error
		for _, k := range keys {
			kh, err := cloneHash(c.h, c.sig.Hash)
			if err != nil {
				lastErr = err
				continue
			}
			if err := k.PublicKey.VerifySignature(kh, c.sig); err != nil {
				lastErr = err
				continue
			}
			out.Status, lastErr = StatusGood, nil
			out.Fingerprint = strings.ToUpper(hex.EncodeToString(k.Entity.PrimaryKey.Fingerprint))
			if n, err := k.PublicKey.BitLength(); err == nil {
				bits = int(n)
			}
			if d := keyProblem(k, c.sig, now); d != "" {
				out.Status, out.Detail = StatusBad, d
			}
			break
		}
		if lastErr != nil {
			out.Status, out.Detail = StatusBad, "it does not verify against the trusted key that claims it: "+lastErr.Error()
		}
	}
	if out.Status == StatusUntrusted {
		out.Detail = "no trusted key made it"
	}
	out.Rejected = pol.rejects(c.sig.Hash, c.sig.PubKeyAlgo, bits)
	return out
}

// keyProblem reports a reason a verified signature still cannot be trusted:
// its key was revoked, or the signature postdates the key's expiry.
func keyProblem(k openpgp.Key, sig *packet.Signature, now time.Time) string {
	if k.Entity.Revoked(now) || len(k.Revocations) > 0 {
		return "the key that made it has been revoked"
	}
	if sig.CreationTime.Before(k.PublicKey.CreationTime) {
		return "it claims to have been made before the key that made it existed"
	}
	if k.SelfSignature != nil && k.PublicKey.KeyExpired(k.SelfSignature, sig.CreationTime) {
		return "it was made after the key that made it expired"
	}
	if sig.SigExpired(now) {
		return "the signature has expired"
	}
	return ""
}

// CheckDetached checks a detached signature — armored or binary, holding one
// or more signature packets — over data, such as repomd.xml.asc over
// repomd.xml. An error means sigData holds no signature at all.
func CheckDetached(data, sigData []byte, keyring openpgp.EntityList, pol Policy) ([]Signature, error) {
	r := io.Reader(bytes.NewReader(sigData))
	if bytes.HasPrefix(bytes.TrimSpace(sigData), []byte("-----BEGIN")) {
		block, err := armor.Decode(bytes.NewReader(sigData))
		if err != nil {
			return nil, fmt.Errorf("decode armored signature: %w", err)
		}
		r = block.Body
	}
	var out []Signature
	packets := packet.NewReader(r)
	now := time.Now()
	for {
		p, err := packets.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			out = append(out, Signature{Tag: "detached", Status: StatusBad, Detail: "malformed OpenPGP signature: " + err.Error()})
			break
		}
		sig, ok := p.(*packet.Signature)
		if !ok {
			continue
		}
		c := newSigCheckPacket("detached", sig)
		c.feed(data)
		s := c.finish(keyring, pol, now)
		s.Readable = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no OpenPGP signature found")
	}
	return out, nil
}

// issuerID returns the key id a signature claims, from the issuer subpacket
// or, for a signature that names only a fingerprint, from that.
func issuerID(sig *packet.Signature) (uint64, bool) {
	if sig.IssuerKeyId != nil {
		return *sig.IssuerKeyId, true
	}
	fp := sig.IssuerFingerprint
	switch {
	case len(fp) == 32: // v6: the key id is the leading 8 bytes
		return binary.BigEndian.Uint64(fp[:8]), true
	case len(fp) >= 8: // v4: the trailing 8 bytes
		return binary.BigEndian.Uint64(fp[len(fp)-8:]), true
	}
	return 0, false
}

func issuerKeyID(sig *packet.Signature) string {
	if id, ok := issuerID(sig); ok {
		return fmt.Sprintf("%016x", id)
	}
	return ""
}

// cloneHash copies a hash's running state, so one pass over the signed data
// can be finished several ways.
func cloneHash(h hash.Hash, alg crypto.Hash) (hash.Hash, error) {
	m, ok := h.(encoding.BinaryMarshaler)
	if !ok {
		return nil, fmt.Errorf("the %s hash state cannot be copied", hashName(alg))
	}
	state, err := m.MarshalBinary()
	if err != nil {
		return nil, err
	}
	n := alg.New()
	u, ok := n.(encoding.BinaryUnmarshaler)
	if !ok {
		return nil, fmt.Errorf("the %s hash state cannot be copied", hashName(alg))
	}
	if err := u.UnmarshalBinary(state); err != nil {
		return nil, err
	}
	return n, nil
}

func hashName(h crypto.Hash) string {
	if h == 0 {
		return "unknown"
	}
	return h.String()
}

func algorithmName(a packet.PublicKeyAlgorithm) string {
	switch a {
	case packet.PubKeyAlgoRSA, packet.PubKeyAlgoRSASignOnly:
		return "RSA"
	case packet.PubKeyAlgoDSA:
		return "DSA"
	case packet.PubKeyAlgoECDSA:
		return "ECDSA"
	case packet.PubKeyAlgoEdDSA:
		return "EdDSA"
	case packet.PubKeyAlgoEd25519:
		return "Ed25519"
	case packet.PubKeyAlgoEd448:
		return "Ed448"
	default:
		return fmt.Sprintf("algorithm %d", a)
	}
}
