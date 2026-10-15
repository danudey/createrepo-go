package pkgcheck

import (
	"bytes"
	"crypto"
	"encoding/binary"
	"encoding/hex"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/danudey/createrepo-go/internal/rpm"
	"github.com/danudey/createrepo-go/internal/rpmtest"
)

// helloRPM is the unsigned reference package the tests mutate.
func helloRPM(t testing.TB) []byte {
	t.Helper()
	data, err := rpmtest.Reference("hello-2.10-3.noarch.rpm")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func split(t testing.TB, b []byte) rpmtest.Parts {
	t.Helper()
	p, err := rpmtest.Split(b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var (
	encodeHeader = rpmtest.EncodeHeader
	assemble     = rpmtest.Assemble
	digestTags   = rpmtest.DigestTags
)

var (
	keyOnce sync.Once
	testKey *openpgp.Entity
	keyErr  error
)

// signingKey is generated once per run: RSA key generation is slow.
func signingKey(t testing.TB) *openpgp.Entity {
	t.Helper()
	keyOnce.Do(func() { testKey, keyErr = rpmtest.NewKey("pkgcheck-test") })
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	return testKey
}

func otherKey(t testing.TB) *openpgp.Entity {
	t.Helper()
	e, err := rpmtest.NewKey("someone-else")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func sign(t testing.TB, e *openpgp.Entity, data []byte, h crypto.Hash) []byte {
	t.Helper()
	sig, err := rpmtest.Sign(e, data, h)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// signedRPM returns the reference package with a v4 header signature
// (RSAHEADER) and/or an rpm 6 OPENPGP signature by e.
func signedRPM(t testing.TB, e *openpgp.Entity, v4, openPGP bool) []byte {
	t.Helper()
	data, err := rpmtest.Signed(helloRPM(t), e, v4, openPGP)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func policy(t testing.TB, target string) Policy {
	t.Helper()
	p, err := PolicyFor(target)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func check(t testing.TB, data []byte, opt Options) *Result {
	t.Helper()
	res, err := Check(bytes.NewReader(data), opt)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return res
}

func TestAnUnsignedPackageIsCleanButUnsigned(t *testing.T) {
	res := check(t, helloRPM(t), Options{})
	if !res.OK() || res.Signed() || len(res.Signatures) != 0 || !res.PayloadChecked {
		t.Fatalf("got %+v; want clean, unsigned, payload checked", res)
	}
	if res.NEVRA != "hello-2.10-3.noarch" {
		t.Errorf("NEVRA = %q", res.NEVRA)
	}
}

func TestATrustedSignatureIsGood(t *testing.T) {
	e := signingKey(t)
	res := check(t, signedRPM(t, e, true, false), Options{Keyring: openpgp.EntityList{e}})
	if !res.OK() || !res.Signed() {
		t.Fatalf("problems %q, signed %v; want clean and signed", res.Problems, res.Signed())
	}
	s := res.Signatures[0]
	if s.Status != StatusGood || s.Tag != "RSAHEADER" || !strings.EqualFold(s.Fingerprint, hex.EncodeToString(e.PrimaryKey.Fingerprint)) {
		t.Errorf("signature = %+v", s)
	}
}

func TestASignatureWithoutItsKeyIsIntactButUntrusted(t *testing.T) {
	res := check(t, signedRPM(t, signingKey(t), true, false), Options{Keyring: openpgp.EntityList{otherKey(t)}})
	if !res.OK() || !res.Signed() {
		t.Fatalf("problems %q; an intact signature by an unknown key is not a defect", res.Problems)
	}
	if s := res.Signatures[0]; s.Status != StatusUntrusted || s.Fingerprint != "" || s.KeyID == "" {
		t.Errorf("signature = %+v; want untrusted with a key id", s)
	}
}

func TestOpenPGPOnlyIsUnsignedForRHEL8And9(t *testing.T) {
	data := signedRPM(t, signingKey(t), false, true)
	for target, signed := range map[string]bool{"": true, TargetRHEL8: false, TargetRHEL9: false, TargetRHEL10: true} {
		res := check(t, data, Options{Policy: policy(t, target)})
		if !res.OK() {
			t.Errorf("%q: problems %q; an unreadable signature is not a defect", target, res.Problems)
		}
		if res.Signed() != signed {
			t.Errorf("%q: Signed() = %v, want %v", target, res.Signed(), signed)
		}
	}
}

func TestASignatureFromAnotherPackageIsBad(t *testing.T) {
	e := signingKey(t)
	p := split(t, helloRPM(t))
	tags := digestTags(p.Header)
	tags[sigTagRSA] = &rpm.Tag{ID: sigTagRSA, Type: rpm.TagTypeBinary, Value: sign(t, e, []byte("another header"), crypto.SHA256)}
	data := assemble(p, tags)

	// Caught without any key, and with one.
	for _, kr := range []openpgp.EntityList{nil, {e}} {
		res := check(t, data, Options{Keyring: kr})
		if res.OK() || !res.Intact() || res.Signed() || res.Signatures[0].Status != StatusBad {
			t.Errorf("keyring %d: problems %q, signatures %+v; want a bad signature on an intact file", len(kr), res.All(), res.Signatures)
		}
	}
}

func TestAForgedSignatureFailsAgainstTheKey(t *testing.T) {
	e := signingKey(t)
	p := split(t, helloRPM(t))
	tags := digestTags(p.Header)
	sig := sign(t, e, p.Header, crypto.SHA256)
	sig[len(sig)-1] ^= 0x01 // damage the RSA value, not the hash tag
	tags[sigTagRSA] = &rpm.Tag{ID: sigTagRSA, Type: rpm.TagTypeBinary, Value: sig}
	data := assemble(p, tags)

	if res := check(t, data, Options{}); !res.OK() {
		t.Fatalf("without a key only the hash tag can be checked, and it matches; got %q", res.Problems)
	}
	if res := check(t, data, Options{Keyring: openpgp.EntityList{e}}); res.OK() {
		t.Error("with the key the forgery must be caught")
	}
}

func TestPolicyRejectsSHA1SignaturesFromRHEL9(t *testing.T) {
	e := signingKey(t)
	p := split(t, helloRPM(t))
	tags := digestTags(p.Header)
	tags[sigTagRSA] = &rpm.Tag{ID: sigTagRSA, Type: rpm.TagTypeBinary, Value: sign(t, e, p.Header, crypto.SHA1)}
	data := assemble(p, tags)

	for target, ok := range map[string]bool{TargetRHEL8: true, TargetRHEL9: false, TargetRHEL10: false} {
		res := check(t, data, Options{Keyring: openpgp.EntityList{e}, Policy: policy(t, target)})
		if res.OK() != ok || res.Signed() != ok {
			t.Errorf("%s: problems %q, signed %v; want ok=%v", target, res.All(), res.Signed(), ok)
		}
	}
}

func TestADamagedPayloadIsCaughtOnlyWhenRead(t *testing.T) {
	data := helloRPM(t)
	data[len(data)-10] ^= 0xff
	res := check(t, data, Options{})
	if res.OK() || !strings.Contains(strings.Join(res.Problems, "\n"), "payload") {
		t.Errorf("problems %q; want a payload digest mismatch", res.Problems)
	}
	if res := check(t, data, Options{HeaderOnly: true}); !res.OK() || res.PayloadChecked {
		t.Errorf("header only: problems %q, payload checked %v; the payload was not read", res.Problems, res.PayloadChecked)
	}
}

func TestADamagedHeaderFailsItsDigestAndSignature(t *testing.T) {
	e := signingKey(t)
	data := signedRPM(t, e, true, false)
	p := split(t, data)
	i := bytes.Index(data, []byte("A friendly greeting"))
	data[i] = 'B'
	_ = p
	res := check(t, data, Options{Keyring: openpgp.EntityList{e}, HeaderOnly: true})
	joined := strings.Join(res.All(), "\n")
	if !strings.Contains(joined, "header SHA-256 digest") || !strings.Contains(joined, "RSAHEADER signature") {
		t.Errorf("problems %q; want the header digest and the signature to fail", res.Problems)
	}
}

func TestEveryTruncationIsReportedNotFatal(t *testing.T) {
	data := helloRPM(t)
	for n := range len(data) {
		res, err := Check(bytes.NewReader(data[:n]), Options{})
		if err != nil {
			t.Fatalf("truncated to %d: error %v; a short file is a defect, not an error", n, err)
		}
		if res.OK() {
			t.Fatalf("truncated to %d: no problem reported", n)
		}
	}
}

func TestFilesThatAreNotPackages(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":       nil,
		"html":        []byte("<html><body>404 Not Found</body></html>" + strings.Repeat(" ", 100)),
		"lfs pointer": []byte("version https://git-lfs.github.com/spec/v1\noid sha256:abc\nsize 12345\n" + strings.Repeat("\n", 60)),
		"zeros":       make([]byte, 8192),
	} {
		res := check(t, data, Options{})
		if res.OK() || !strings.Contains(res.Problems[0], "not an RPM") {
			t.Errorf("%s: problems %q; want not an RPM", name, res.Problems)
		}
	}
}

func TestForgedHeaderSizesAreRefusedBeforeAllocating(t *testing.T) {
	for _, off := range []int{96 + 8, 96 + 12} { // signature header entry count, data size
		data := helloRPM(t)
		binary.BigEndian.PutUint32(data[off:], 0xFFFFFFFF)
		if res := check(t, data, Options{}); res.OK() {
			t.Errorf("offset %d: a 4 GiB header was accepted", off)
		}
	}
}

func TestInconsistentDependencyArraysAreReportedNotFatal(t *testing.T) {
	p := split(t, helloRPM(t))
	h, err := rpm.ParseHeader(p.Header)
	if err != nil {
		t.Fatal(err)
	}
	names := h.GetTag(1049) // REQUIRENAME
	names.Value = append(slices.Clone(names.StringSlice()), "extra")
	p.Header = encodeHeader(h.Tags)
	res := check(t, assemble(p, digestTags(p.Header)), Options{HeaderOnly: true})
	if res.OK() || !strings.Contains(strings.Join(res.Problems, "\n"), "requires arrays disagree") {
		t.Errorf("problems %q; want the requires arrays reported", res.Problems)
	}
}

func TestDetachedSignatures(t *testing.T) {
	e := signingKey(t)
	data := []byte("<repomd/>\n")
	var armored bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&armored, e, bytes.NewReader(data), nil); err != nil {
		t.Fatal(err)
	}
	sigs, err := CheckDetached(data, armored.Bytes(), openpgp.EntityList{e}, Policy{})
	if err != nil || len(sigs) != 1 || sigs[0].Status != StatusGood {
		t.Fatalf("sigs %+v, err %v; want one good signature", sigs, err)
	}
	sigs, _ = CheckDetached([]byte("<repomd>changed</repomd>\n"), armored.Bytes(), nil, Policy{})
	if sigs[0].Status != StatusBad {
		t.Errorf("a signature over other data = %+v; want bad", sigs[0])
	}
	if sigs, err := CheckDetached(data, []byte("not a signature"), nil, Policy{}); err == nil && sigs[0].Status != StatusBad {
		t.Errorf("garbage was accepted as a signature: %+v", sigs)
	}
}

// FuzzCheck asserts that no input makes the checker panic or hang.
func FuzzCheck(f *testing.F) {
	f.Add(helloRPM(f))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		if _, err := Check(bytes.NewReader(data), Options{}); err != nil {
			t.Fatalf("a reader over memory cannot fail, got %v", err)
		}
	})
}
