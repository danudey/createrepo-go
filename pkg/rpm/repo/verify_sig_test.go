package repo

import (
	"bytes"
	"context"
	"crypto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/danudey/createrepo-go/internal/rpmtest"
	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/rpm/pkgcheck"
)

var (
	sigKeysOnce sync.Once
	sigKeyA     *openpgp.Entity
	sigKeyB     *openpgp.Entity
	sigKeysErr  error
)

// sigKeys returns two signing keys, generated once per run.
func sigKeys(t *testing.T) (a, b *openpgp.Entity) {
	t.Helper()
	sigKeysOnce.Do(func() {
		if sigKeyA, sigKeysErr = rpmtest.NewKey("key-a"); sigKeysErr == nil {
			sigKeyB, sigKeysErr = rpmtest.NewKey("key-b")
		}
	})
	if sigKeysErr != nil {
		t.Fatal(sigKeysErr)
	}
	return sigKeyA, sigKeyB
}

// signing says how to sign one reference package; a nil key leaves it
// unsigned.
type signing struct {
	key         *openpgp.Entity
	v4, openPGP bool
}

func v4(e *openpgp.Entity) signing { return signing{key: e, v4: true} }

// entitySigner signs repomd.xml with a go-crypto key.
type entitySigner struct{ e *openpgp.Entity }

func (s entitySigner) SignDetached(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	err := openpgp.ArmoredDetachSign(&buf, s.e, bytes.NewReader(data), nil)
	return buf.Bytes(), err
}

// signedRepo publishes hello and libfoo, signed as given, into a new local
// repository, signing repomd.xml with metadataKey when it is set.
func signedRepo(t *testing.T, hello, libfoo signing, metadataKey *openpgp.Entity) string {
	t.Helper()
	dir := t.TempDir()
	src := t.TempDir()
	opts := Options{Create: true, now: 1000}
	if metadataKey != nil {
		opts.Signer = entitySigner{metadataKey}
	}
	r, _ := newLocalRepoWithOpts(t, dir, opts)
	defer r.Close()
	for name, how := range map[string]signing{"hello-2.10-3.noarch.rpm": hello, "libfoo-1.3.0-1.x86_64.rpm": libfoo} {
		data, err := rpmtest.Reference(name)
		if err != nil {
			t.Fatal(err)
		}
		if how.key != nil {
			if data, err = rpmtest.Signed(data, how.key, how.v4, how.openPGP); err != nil {
				t.Fatal(err)
			}
		}
		path := filepath.Join(src, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := r.AddRPM(path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	return dir
}

func verifyDir(t *testing.T, dir string, opt VerifyOptions) *VerifyResult {
	t.Helper()
	be, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return openVerify(t, be, opt)
}

// details flattens a result's problems for matching.
func details(ps []Problem) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(p.String())
		b.WriteByte('\n')
	}
	return b.String()
}

func TestVerifyAcceptsARepositorySignedByOneTrustedKey(t *testing.T) {
	a, _ := sigKeys(t)
	dir := signedRepo(t, v4(a), v4(a), a)
	res := verifyDir(t, dir, VerifyOptions{Keyring: openpgp.EntityList{a}, RequireTrusted: true, RequireSignedPackages: true, RequireSignedMetadata: true})
	if !res.OK() {
		t.Fatalf("problems:\n%s", details(res.Problems))
	}
	if len(res.Signers) != 1 || res.Unsigned != 0 || res.HeadersChecked != 2 || res.PayloadsChecked != 2 {
		t.Errorf("signers %v, unsigned %d, headers %d, payloads %d", res.Signers, res.Unsigned, res.HeadersChecked, res.PayloadsChecked)
	}
	for id := range res.Signers {
		if !res.Trusted[id] {
			t.Errorf("signer %s not marked trusted", id)
		}
	}
	if !res.Metadata.Present || res.Metadata.Signatures[0].Status != pkgcheck.StatusGood {
		t.Errorf("metadata signature = %+v", res.Metadata)
	}
}

func TestVerifyChecksSignaturesWithoutBeingAsked(t *testing.T) {
	a, _ := sigKeys(t)
	dir := signedRepo(t, v4(a), v4(a), nil)
	if res := verifyDir(t, dir, VerifyOptions{}); !res.OK() || len(res.Signers) != 1 {
		t.Fatalf("an intact signature by an unknown key is fine by default: %s, signers %v", details(res.Problems), res.Signers)
	}

	// Swap one package for a copy whose signature belongs to another header:
	// the file checksum in the metadata is updated to match, so only the
	// signature check can tell.
	path := filepath.Join(dir, "hello-2.10-3.noarch.rpm")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := rpmtest.Split(data)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := rpmtest.Reference("libfoo-1.3.0-1.x86_64.rpm")
	op, _ := rpmtest.Split(other)
	tags := rpmtest.DigestTags(p.Header)
	sig, _ := rpmtest.Sign(a, op.Header, crypto.SHA256)
	tags[rpmtest.TagRSAHeader] = p.Sig.GetTag(rpmtest.TagRSAHeader)
	tags[rpmtest.TagRSAHeader].Value = sig
	writeAndReindex(t, dir, path, rpmtest.Assemble(p, tags))

	res := verifyDir(t, dir, VerifyOptions{})
	if res.OK() || !strings.Contains(details(res.Problems), "RSAHEADER signature by key") {
		t.Fatalf("problems:\n%s\nwant the copied signature reported", details(res.Problems))
	}
}

func TestVerifyRequiresEveryPackageSignedByTheSameKey(t *testing.T) {
	a, b := sigKeys(t)

	res := verifyDir(t, signedRepo(t, v4(a), v4(b), nil), VerifyOptions{})
	if got := strings.Count(details(res.Problems), "every package should be signed by the same key"); got != 1 {
		t.Errorf("two keys: problems:\n%s\nwant one package named", details(res.Problems))
	}

	res = verifyDir(t, signedRepo(t, v4(a), signing{}, nil), VerifyOptions{})
	if !strings.Contains(details(res.Problems), "libfoo-1.3.0-1.x86_64: it is not signed, but 1 other package(s)") {
		t.Errorf("one unsigned: problems:\n%s", details(res.Problems))
	}

	// The key the repository records wins, however few packages use it.
	res = verifyDir(t, signedRepo(t, v4(a), v4(b), nil), VerifyOptions{ExpectedKeyIDs: []string{keyID(b)}})
	if d := details(res.Problems); !strings.Contains(d, "hello-2.10-3.noarch: the RSAHEADER signature is by key "+keyID(a)+", not the repository's signing key") {
		t.Errorf("expected key: problems:\n%s", d)
	}

	// When the recorded key could not be expanded, another key id may be one
	// of its subkeys: say so, but do not fail on it.
	res = verifyDir(t, signedRepo(t, v4(a), v4(a), nil), VerifyOptions{ExpectedKeyIDs: []string{keyID(b)}, ExpectedKeyIncomplete: true})
	if !res.OK() || strings.Count(details(res.Warnings), "unless it is one of its subkeys") != 2 {
		t.Errorf("incomplete expected key: problems:\n%s\nwarnings:\n%s", details(res.Problems), details(res.Warnings))
	}
}

func TestVerifyRequireFlags(t *testing.T) {
	a, b := sigKeys(t)
	unsigned := signedRepo(t, signing{}, signing{}, nil)
	if res := verifyDir(t, unsigned, VerifyOptions{}); !res.OK() || res.Unsigned != 2 {
		t.Fatalf("an unsigned repository is not a problem unless signatures are required: %s", details(res.Problems))
	}
	res := verifyDir(t, unsigned, VerifyOptions{RequireSignedPackages: true, RequireSignedMetadata: true})
	d := details(res.Problems)
	if strings.Count(d, "it is not signed") != 2 || !strings.Contains(d, "there is no repomd.xml.asc") {
		t.Errorf("required signatures missing: problems:\n%s", d)
	}

	// Signed, but by a key the keyring does not hold.
	res = verifyDir(t, signedRepo(t, v4(a), v4(a), a), VerifyOptions{Keyring: openpgp.EntityList{b}, RequireTrusted: true})
	d = details(res.Problems)
	if strings.Count(d, "which is not a trusted key") != 3 { // two packages and repomd.xml
		t.Errorf("untrusted key: problems:\n%s", d)
	}
}

func TestVerifyJudgesSignaturesAsTheTargetReadsThem(t *testing.T) {
	a, _ := sigKeys(t)
	dir := signedRepo(t, signing{key: a, openPGP: true}, signing{key: a, openPGP: true}, nil)
	rhel8, _ := pkgcheck.PolicyFor(pkgcheck.TargetRHEL8)
	rhel10, _ := pkgcheck.PolicyFor(pkgcheck.TargetRHEL10)

	if res := verifyDir(t, dir, VerifyOptions{Policy: rhel10, RequireSignedPackages: true}); !res.OK() {
		t.Errorf("rhel10 reads OPENPGP: %s", details(res.Problems))
	}
	res := verifyDir(t, dir, VerifyOptions{Policy: rhel8})
	if !res.OK() || strings.Count(details(res.Warnings), "which rhel8 cannot read") != 2 {
		t.Errorf("rhel8, not required: problems:\n%s\nwarnings:\n%s", details(res.Problems), details(res.Warnings))
	}
	res = verifyDir(t, dir, VerifyOptions{Policy: rhel8, RequireSignedPackages: true})
	if strings.Count(details(res.Problems), "which rhel8 cannot read") != 2 {
		t.Errorf("rhel8, required: problems:\n%s", details(res.Problems))
	}
}

func TestVerifyFindsDamageTheMetadataAgreesWith(t *testing.T) {
	dir, ids := verifiableRepo(t)
	path := rpmPath(t, dir, ids, "hello")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-10] ^= 0xff // the payload
	writeAndReindex(t, dir, path, data)

	res := verifyDir(t, dir, VerifyOptions{})
	if res.OK() || !strings.Contains(details(res.Problems), "payload") {
		t.Fatalf("problems:\n%s\nwant the payload digest mismatch, though size and checksum agree", details(res.Problems))
	}
}

func TestVerifyReadsOnlyHeadersFromRemoteStorage(t *testing.T) {
	a, _ := sigKeys(t)
	dir := signedRepo(t, v4(a), v4(a), nil)
	inner, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	remote := &recordingBackend{Backend: inner, recorded: map[string]string{}}

	res := openVerify(t, remote, VerifyOptions{Keyring: openpgp.EntityList{a}, RequireTrusted: true})
	if !res.OK() || res.HeadersChecked != 2 || res.PayloadsChecked != 0 || res.Downloaded != 0 {
		t.Errorf("cheap: problems %s; headers %d payloads %d downloaded %d", details(res.Problems), res.HeadersChecked, res.PayloadsChecked, res.Downloaded)
	}
	res = openVerify(t, remote, VerifyOptions{Checksums: ChecksumContent, Keyring: openpgp.EntityList{a}})
	if !res.OK() || res.PayloadsChecked != 2 || res.Downloaded != 2 {
		t.Errorf("content: problems %s; payloads %d downloaded %d; the download must serve both checks", details(res.Problems), res.PayloadsChecked, res.Downloaded)
	}
}

func TestVerifyReportsABadMetadataSignature(t *testing.T) {
	a, _ := sigKeys(t)
	dir := signedRepo(t, signing{}, signing{}, a)
	// Re-sign a different document in place of repomd.xml.
	asc, err := entitySigner{a}.SignDetached([]byte("<repomd/>"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repodata", "repomd.xml.asc"), asc, 0o644); err != nil {
		t.Fatal(err)
	}
	res := verifyDir(t, dir, VerifyOptions{})
	if !strings.Contains(details(res.Problems), "the repomd.xml signature is bad") {
		t.Errorf("problems:\n%s", details(res.Problems))
	}
}

// writeAndReindex replaces the package at path and updates the metadata to
// the new file's size and checksum, leaving a repository whose metadata
// agrees with whatever the file now holds.
func writeAndReindex(t *testing.T, dir, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	r, _ := newLocalRepoWithOpts(t, dir, Options{now: 3000})
	defer r.Close()
	rel, _ := filepath.Rel(dir, path)
	for _, p := range r.Index().Packages() {
		if p.Location != filepath.ToSlash(rel) {
			continue
		}
		fresh := *p
		fresh.PkgID = sha256Hex(data)
		fresh.SizePackage = int64(len(data))
		r.idx.RemoveByPkgID(p.PkgID)
		r.idx.Add(&fresh)
	}
	r.original = map[string]string{} // nothing is unreferenced: keep every file
	if _, err := r.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func keyID(e *openpgp.Entity) string {
	return strings.ToLower(e.PrimaryKey.KeyIdString())
}
