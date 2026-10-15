// Package pkgcheck checks an RPM package file against itself: that it parses,
// that the digests it carries match its header and payload, and that the
// OpenPGP signatures it carries are intact and — given trusted keys — made by
// one of them. It needs no rpm installation, and reads the package in a single
// pass so it can inspect a package as it streams from remote storage.
//
// What rpm checks is not the same on every release: rpm 4.14 and 4.16 (RHEL 8
// and 9) cannot read the rpm 6 OPENPGP signature tag, and RHEL 9 onwards
// refuses SHA-1 and DSA signatures. A Policy states one release's rules, so a
// package can be judged as that release's clients will judge it.
package pkgcheck

import (
	"bytes"
	"crypto"
	"crypto/md5"  //nolint:gosec // rpm's legacy header+payload digest is MD5
	"crypto/sha1" //nolint:gosec // rpm's legacy header digest is SHA-1
	"crypto/sha256"
	"crypto/sha3"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/danudey/createrepo-go/internal/rpm"
)

// Signature-header tags (rpmtag.h, RPMSIGTAG_*).
const (
	sigTagDSA      = 267 // OpenPGP signature over the header
	sigTagRSA      = 268 // OpenPGP signature over the header
	sigTagSHA1     = 269 // hex SHA-1 of the header
	sigTagLongSize = 270 // header+payload size, 64-bit
	sigTagSHA256   = 273 // hex SHA-256 of the header
	sigTagOpenPGP  = 278 // rpm 6: base64 OpenPGP signatures over the header
	sigTagSHA3_256 = 279 // rpm 6: hex SHA3-256 of the header
	sigTagSize     = 1000
	sigTagPGP      = 1002 // OpenPGP signature over header+payload
	sigTagMD5      = 1004 // MD5 of header+payload
	sigTagGPG      = 1005 // OpenPGP signature over header+payload
)

// Main-header payload digest tags.
const (
	tagPayloadDigest     = 5092 // hex digest of the compressed payload
	tagPayloadDigestAlgo = 5093 // its OpenPGP hash algorithm id (default SHA-256)
	tagPayloadSHA512     = 5121 // rpm 6
	tagPayloadSHA3_256   = 5123 // rpm 6
)

// leadSize is the fixed size of the legacy lead; leadSigTypeHeader is the only
// signature type rpm has written since rpm 3.
const (
	leadSize          = 96
	leadSigTypeHeader = 5
	maxHeaderBytes    = 32 << 20
)

// Options configures a check.
type Options struct {
	// Keyring holds the trusted public keys. Without one, signatures are
	// checked for integrity only.
	Keyring openpgp.EntityList
	// Policy is the release whose rules the signatures are judged by.
	Policy Policy
	// HeaderOnly stops after the main header: the header digests and header
	// signatures are checked, but the payload is not read, so its digests
	// and the legacy header+payload signatures go unchecked. It is for a
	// package that would otherwise have to be downloaded in full.
	HeaderOnly bool
}

// Result is what a check found.
type Result struct {
	// Problems are defects in the file itself: it does not parse, or a
	// digest does not match.
	Problems []string
	// SignatureProblems are bad signatures, and signatures the target's
	// policy refuses. They are kept apart because replacing a package's
	// signatures cures them, where nothing cures a damaged file.
	SignatureProblems []string
	// Signatures lists every signature the package carries.
	Signatures []Signature
	// PayloadChecked reports whether the payload was read and its digests
	// checked.
	PayloadChecked bool
	// NEVRA identifies the package, when its header could be read.
	NEVRA string
}

// OK reports whether the package has no problems of either kind.
func (r *Result) OK() bool { return len(r.Problems) == 0 && len(r.SignatureProblems) == 0 }

// Intact reports whether the file itself is sound, whatever its signatures.
func (r *Result) Intact() bool { return len(r.Problems) == 0 }

// All returns every problem, file defects first.
func (r *Result) All() []string {
	return append(slices.Clone(r.Problems), r.SignatureProblems...)
}

// Signed reports whether at least one signature makes the package signed for
// the policy's release.
func (r *Result) Signed() bool {
	for _, s := range r.Signatures {
		if s.Counts() {
			return true
		}
	}
	return false
}

// Err summarizes the problems as one error, or returns nil.
func (r *Result) Err() error {
	if r.OK() {
		return nil
	}
	return errors.New(strings.Join(r.All(), "; "))
}

// CheckFile checks the package at path.
func CheckFile(path string, opt Options) (*Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Check(f, opt)
}

// errStop marks a defect after which the rest of the file cannot be read.
var errStop = errors.New("stop")

// Check reads a package from r and checks it. Defects in the package are
// reported in the Result; an error is returned only when r itself fails.
func Check(r io.Reader, opt Options) (*Result, error) {
	c := &checker{opt: opt, res: &Result{}, now: time.Now()}
	err := c.run(r)
	if err != nil && !errors.Is(err, errStop) {
		return nil, err
	}
	return c.res, nil
}

type checker struct {
	opt Options
	res *Result
	now time.Time

	sig    *rpm.Header
	header []byte // the main header, exactly as stored
	hdr    *rpm.Header
}

func (c *checker) problem(format string, a ...any) {
	c.res.Problems = append(c.res.Problems, fmt.Sprintf(format, a...))
}

// fail records a defect that ends the check.
func (c *checker) fail(format string, a ...any) error {
	c.problem(format, a...)
	return errStop
}

// read fills b from r, turning a short read into a reported truncation.
func (c *checker) read(r io.Reader, b []byte, what string) error {
	if _, err := io.ReadFull(r, b); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return c.fail("the file is truncated inside the %s", what)
		}
		return err
	}
	return nil
}

func (c *checker) run(r io.Reader) error {
	lead := make([]byte, leadSize)
	if _, err := io.ReadFull(r, lead); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return c.fail("not an RPM package: the file is shorter than an RPM lead")
		}
		return err
	}
	l, err := rpm.ParseLead(lead)
	if errors.Is(err, rpm.ErrNotRPMFile) {
		return c.fail("not an RPM package: the file does not start with the RPM magic number")
	}
	if err != nil {
		return c.fail("invalid RPM lead: %v", err)
	}
	if l.SignatureType != leadSigTypeHeader {
		return c.fail("invalid RPM lead: signature type %d (rpm writes only %d)", l.SignatureType, leadSigTypeHeader)
	}

	sigRaw, err := c.readHeader(r, "signature header")
	if err != nil {
		return err
	}
	if pad := (8 - len(sigRaw)%8) % 8; pad > 0 {
		if err := c.read(r, make([]byte, pad), "signature header padding"); err != nil {
			return err
		}
	}
	if c.sig, err = rpm.ParseHeader(sigRaw); err != nil {
		return c.fail("invalid signature header: %v", err)
	}

	if c.header, err = c.readHeader(r, "main header"); err != nil {
		return err
	}
	if c.hdr, err = rpm.ParseHeader(c.header); err != nil {
		return c.fail("invalid main header: %v", err)
	}
	pkg := &rpm.Package{Lead: *l, Signature: *c.sig, Header: *c.hdr}
	c.res.NEVRA = pkg.String()
	if err := pkg.Validate(); err != nil {
		c.problem("invalid main header: %v", err)
	}

	c.checkHeaderDigests()
	headerSigs, payloadSigs := c.signatures()
	for _, s := range headerSigs {
		s.feed(c.header)
	}

	if !c.opt.HeaderOnly {
		if err := c.checkPayload(r, payloadSigs); err != nil {
			return err
		}
	}
	for _, s := range headerSigs {
		c.addSignature(s.finish(c.opt.Keyring, c.opt.Policy, c.now))
	}
	for _, s := range payloadSigs {
		if c.opt.HeaderOnly {
			out := s.out
			if s.h != nil {
				out.Status, out.Detail = StatusUnchecked, "it covers the payload, which was not read"
			}
			c.addSignature(out)
			continue
		}
		c.addSignature(s.finish(c.opt.Keyring, c.opt.Policy, c.now))
	}
	return nil
}

// readHeader reads one header structure (no padding), returning its bytes.
func (c *checker) readHeader(r io.Reader, what string) ([]byte, error) {
	intro := make([]byte, 16)
	if err := c.read(r, intro, what); err != nil {
		return nil, err
	}
	if !bytes.Equal(intro[:3], []byte{0x8E, 0xAD, 0xE8}) {
		return nil, c.fail("invalid %s: bad magic number", what)
	}
	entries := uint64(binary.BigEndian.Uint32(intro[8:12]))
	size := uint64(binary.BigEndian.Uint32(intro[12:16]))
	if entries*16 > maxHeaderBytes || size > maxHeaderBytes {
		return nil, c.fail("invalid %s: it claims %d entries and %d bytes of data, over rpm's %d-byte limit", what, entries, size, maxHeaderBytes)
	}
	// Read what the header claims without allocating it up front: the claim
	// is untrusted, and a short file should cost what it holds, not 64 MiB.
	want := int64(entries*16 + size)
	rest, err := io.ReadAll(io.LimitReader(r, want))
	if err != nil {
		return nil, err
	}
	if int64(len(rest)) != want {
		return nil, c.fail("the file is truncated inside the %s", what)
	}
	raw := make([]byte, 0, len(intro)+len(rest))
	return append(append(raw, intro...), rest...), nil
}

// checkHeaderDigests compares the header digests in the signature header with
// the main header.
func (c *checker) checkHeaderDigests() {
	digests := []struct {
		tag  int
		name string
		h    func() hash.Hash
	}{
		{sigTagSHA256, "SHA-256", sha256.New},
		{sigTagSHA1, "SHA-1", sha1.New},
		{sigTagSHA3_256, "SHA3-256", func() hash.Hash { return sha3.New256() }},
	}
	any := false
	for _, d := range digests {
		t := c.sig.GetTag(d.tag)
		if t == nil {
			continue
		}
		any = true
		h := d.h()
		h.Write(c.header)
		if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, t.String()) {
			c.problem("the header %s digest does not match: the package records %s, the header hashes to %s", d.name, t.String(), got)
		}
	}
	if !any {
		c.problem("the package carries no header digest")
	}
}

// signatures collects the package's signatures: those over the header alone,
// and the legacy ones over the header and payload.
func (c *checker) signatures() (header, payload []*sigCheck) {
	for _, s := range []struct {
		tag  int
		name string
	}{{sigTagRSA, "RSAHEADER"}, {sigTagDSA, "DSAHEADER"}} {
		if raw := c.sig.GetTag(s.tag).Bytes(); raw != nil {
			header = append(header, c.readable(newSigCheck(s.name, raw), true))
		}
	}
	for i, enc := range c.sig.GetTag(sigTagOpenPGP).StringSlice() {
		raw, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			header = append(header, &sigCheck{out: Signature{
				Tag: "OPENPGP", Status: StatusBad,
				Detail: fmt.Sprintf("signature %d is not valid base64: %v", i+1, err),
			}})
			continue
		}
		header = append(header, c.readable(newSigCheck("OPENPGP", raw), c.opt.Policy.ReadsOpenPGPTag))
	}
	for _, s := range []struct {
		tag  int
		name string
	}{{sigTagPGP, "PGP"}, {sigTagGPG, "GPG"}} {
		if raw := c.sig.GetTag(s.tag).Bytes(); raw != nil {
			sc := c.readable(newSigCheck(s.name, raw), true)
			sc.feed(c.header)
			payload = append(payload, sc)
		}
	}
	return header, payload
}

func (c *checker) readable(s *sigCheck, readable bool) *sigCheck {
	s.out.Readable = readable
	return s
}

// addSignature records a checked signature, and a problem for a bad one or one
// the policy refuses.
func (c *checker) addSignature(s Signature) {
	// A signature the release cannot read is not a defect in itself: callers
	// decide whether a package signed only that way is acceptable. One that
	// is damaged is, whoever reads it.
	c.res.Signatures = append(c.res.Signatures, s)
	switch {
	case s.Status == StatusBad:
		c.res.SignatureProblems = append(c.res.SignatureProblems, fmt.Sprintf("the %s is bad: %s", sigName(s), s.Detail))
	case s.Readable && s.Rejected != "" && s.Status != StatusUnsupported:
		c.res.SignatureProblems = append(c.res.SignatureProblems, fmt.Sprintf("the %s will be refused: %s", sigName(s), s.Rejected))
	}
}

func sigName(s Signature) string {
	if s.KeyID == "" {
		return s.Tag + " signature"
	}
	return fmt.Sprintf("%s signature by key %s", s.Tag, s.Signer())
}

// payloadDigest is one digest of the payload the header records.
type payloadDigest struct {
	name string
	want string
	h    hash.Hash
}

// checkPayload streams the payload, checking its digests, the header+payload
// MD5 and size, and feeding the legacy signatures.
func (c *checker) checkPayload(r io.Reader, sigs []*sigCheck) error {
	var digests []payloadDigest
	if want := c.hdr.GetTag(tagPayloadDigest).StringSlice(); len(want) > 0 {
		algo := crypto.SHA256
		if t := c.hdr.GetTag(tagPayloadDigestAlgo); t != nil {
			a, ok := openPGPHash(t.Int64())
			if !ok || !a.Available() {
				c.problem("the payload digest uses hash algorithm %d, which is not supported", t.Int64())
			} else {
				algo = a
			}
		}
		if algo.Available() {
			digests = append(digests, payloadDigest{name: hashName(algo), want: want[0], h: algo.New()})
		}
	}
	if want := c.hdr.GetTag(tagPayloadSHA512).StringSlice(); len(want) > 0 {
		digests = append(digests, payloadDigest{name: "SHA-512", want: want[0], h: sha512.New()})
	}
	if want := c.hdr.GetTag(tagPayloadSHA3_256).StringSlice(); len(want) > 0 {
		digests = append(digests, payloadDigest{name: "SHA3-256", want: want[0], h: sha3.New256()})
	}
	md5Want := c.sig.GetTag(sigTagMD5).Bytes()
	md5h := md5.New() //nolint:gosec // matching rpm's legacy digest
	md5h.Write(c.header)

	writers := []io.Writer{md5h}
	for _, d := range digests {
		writers = append(writers, d.h)
	}
	for _, s := range sigs {
		writers = append(writers, s)
	}
	n, err := io.Copy(io.MultiWriter(writers...), r)
	if err != nil {
		return err
	}
	c.res.PayloadChecked = true

	if n == 0 {
		c.problem("the package has no payload")
	}
	for _, d := range digests {
		if got := hex.EncodeToString(d.h.Sum(nil)); !strings.EqualFold(got, d.want) {
			c.problem("the payload %s digest does not match: the header records %s, the payload hashes to %s", d.name, d.want, got)
		}
	}
	if md5Want != nil {
		if got := md5h.Sum(nil); !bytes.Equal(got, md5Want) {
			c.problem("the header+payload MD5 digest does not match: the package records %x, the content hashes to %x", md5Want, got)
		}
	}
	if len(digests) == 0 && md5Want == nil {
		c.problem("the package carries no payload digest")
	}

	total := int64(len(c.header)) + n
	if t := c.sig.GetTag(sigTagLongSize); t != nil && t.Int64() != total {
		c.problem("the recorded header+payload size %d does not match the actual %d", t.Int64(), total)
	} else if t := c.sig.GetTag(sigTagSize); t != nil && t.Int64() != total {
		c.problem("the recorded header+payload size %d does not match the actual %d", t.Int64(), total)
	}
	return nil
}

// openPGPHash maps an OpenPGP hash algorithm id (RFC 9580 §9.5), as rpm
// records it, to a Go hash.
func openPGPHash(id int64) (crypto.Hash, bool) {
	switch id {
	case 1:
		return crypto.MD5, true
	case 2:
		return crypto.SHA1, true
	case 8:
		return crypto.SHA256, true
	case 9:
		return crypto.SHA384, true
	case 10:
		return crypto.SHA512, true
	case 11:
		return crypto.SHA224, true
	case 12:
		return crypto.SHA3_256, true
	case 14:
		return crypto.SHA3_512, true
	}
	return 0, false
}
