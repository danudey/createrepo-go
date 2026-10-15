// Package rpmtest builds RPM packages for tests: signed, re-signed, or damaged
// in one precise way, from a real package and without rpmbuild or rpmsign, so
// the tests that use them run anywhere.
package rpmtest

import (
	"bytes"
	"crypto"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/danudey/createrepo-go/internal/rpm"
)

// Signature-header tags the builders write.
const (
	TagRSAHeader    = 268
	TagSHA256Header = 273
	TagOpenPGP      = 278
)

// Reference returns the bytes of a committed reference package, by file name
// ("hello-2.10-3.noarch.rpm", "libfoo-1.3.0-1.x86_64.rpm").
func Reference(name string) ([]byte, error) {
	_, file, _, _ := runtime.Caller(0)
	return os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "reference", "rpm", "repo", name))
}

// Parts is a package split into the pieces a builder reassembles.
type Parts struct {
	Lead    []byte
	Sig     *rpm.Header
	Header  []byte // the main header, exactly as stored
	Payload []byte
}

// Split divides a package into its lead, signature header, main header and
// payload.
func Split(b []byte) (Parts, error) {
	if len(b) < 96+16 {
		return Parts{}, fmt.Errorf("too short to be a package")
	}
	sigLen := 16 + 16*int(binary.BigEndian.Uint32(b[96+8:])) + int(binary.BigEndian.Uint32(b[96+12:]))
	sig, err := rpm.ParseHeader(b[96 : 96+sigLen])
	if err != nil {
		return Parts{}, err
	}
	start := 96 + sigLen + (8-sigLen%8)%8
	hdrLen := 16 + 16*int(binary.BigEndian.Uint32(b[start+8:])) + int(binary.BigEndian.Uint32(b[start+12:]))
	return Parts{
		Lead:    slices.Clone(b[:96]),
		Sig:     sig,
		Header:  slices.Clone(b[start : start+hdrLen]),
		Payload: slices.Clone(b[start+hdrLen:]),
	}, nil
}

// EncodeHeader serializes tags into a header structure. Region tags (62, 63)
// are left out: their offsets describe the original layout, and nothing these
// tests exercise reads them.
func EncodeHeader(tags map[int]*rpm.Tag) []byte {
	ids := make([]int, 0, len(tags))
	for id := range tags {
		if id != 62 && id != 63 {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	var index, store bytes.Buffer
	for _, id := range ids {
		tag := tags[id]
		align := map[rpm.TagType]int{rpm.TagTypeInt16: 2, rpm.TagTypeInt32: 4, rpm.TagTypeInt64: 8}[tag.Type]
		for align > 0 && store.Len()%align != 0 {
			store.WriteByte(0)
		}
		off, count := store.Len(), 0
		switch v := tag.Value.(type) {
		case []byte:
			store.Write(v)
			count = len(v)
		case []string:
			for _, s := range v {
				store.WriteString(s)
				store.WriteByte(0)
			}
			count = len(v)
		case []int64:
			for _, n := range v {
				var b [8]byte
				binary.BigEndian.PutUint64(b[:], uint64(n)) //nolint:gosec // test data: the bit pattern is the point
				store.Write(b[8-align:])
			}
			count = len(v)
		}
		for _, n := range []int{id, int(tag.Type), off, count} {
			_ = binary.Write(&index, binary.BigEndian, uint32(n)) //nolint:gosec // test headers are small
		}
	}
	out := []byte{0x8E, 0xAD, 0xE8, 0x01, 0, 0, 0, 0}
	out = binary.BigEndian.AppendUint32(out, uint32(len(ids)))    //nolint:gosec // test headers are small
	out = binary.BigEndian.AppendUint32(out, uint32(store.Len())) //nolint:gosec // test headers are small
	out = append(out, index.Bytes()...)
	return append(out, store.Bytes()...)
}

// Assemble builds a package from p's lead, header and payload and a
// signature header holding sigTags.
func Assemble(p Parts, sigTags map[int]*rpm.Tag) []byte {
	sig := EncodeHeader(sigTags)
	out := slices.Clone(p.Lead)
	out = append(out, sig...)
	out = append(out, make([]byte, (8-len(sig)%8)%8)...)
	out = append(out, p.Header...)
	return append(out, p.Payload...)
}

// DigestTags returns signature tags carrying a correct SHA-256 header digest.
func DigestTags(header []byte) map[int]*rpm.Tag {
	sum := sha256.Sum256(header)
	return map[int]*rpm.Tag{
		TagSHA256Header: {ID: TagSHA256Header, Type: rpm.TagTypeString, Value: []string{hex.EncodeToString(sum[:])}},
	}
}

// NewKey generates a 2048-bit RSA signing key.
func NewKey(name string) (*openpgp.Entity, error) {
	return openpgp.NewEntity(name, "", name+"@example.com", &packet.Config{RSABits: 2048})
}

// Sign makes a binary detached signature over data with hash h. It builds the
// packet itself because go-crypto refuses to sign with SHA-1, which tests of
// crypto policy need.
func Sign(e *openpgp.Entity, data []byte, h crypto.Hash) ([]byte, error) {
	sig := &packet.Signature{
		Version:      4,
		SigType:      packet.SigTypeBinary,
		PubKeyAlgo:   e.PrivateKey.PubKeyAlgo,
		Hash:         h,
		CreationTime: time.Now(),
		IssuerKeyId:  &e.PrivateKey.KeyId,
	}
	hh := h.New()
	hh.Write(data)
	if err := sig.Sign(hh, e.PrivateKey, &packet.Config{DefaultHash: h, NonDeterministicSignaturesViaNotation: new(false)}); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := sig.Serialize(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Signed returns base re-signed by e: with a v4 header signature (RSAHEADER),
// an rpm 6 OPENPGP signature, or both. Any signature base carried is dropped.
func Signed(base []byte, e *openpgp.Entity, v4, openPGP bool) ([]byte, error) {
	p, err := Split(base)
	if err != nil {
		return nil, err
	}
	tags := DigestTags(p.Header)
	sig, err := Sign(e, p.Header, crypto.SHA256)
	if err != nil {
		return nil, err
	}
	if v4 {
		tags[TagRSAHeader] = &rpm.Tag{ID: TagRSAHeader, Type: rpm.TagTypeBinary, Value: sig}
	}
	if openPGP {
		tags[TagOpenPGP] = &rpm.Tag{ID: TagOpenPGP, Type: rpm.TagTypeStringArray, Value: []string{base64.StdEncoding.EncodeToString(sig)}}
	}
	return Assemble(p, tags), nil
}
