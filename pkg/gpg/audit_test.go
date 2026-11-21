package gpg

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// auditKey generates a signing key: a primary key (which certifies and signs)
// and an encryption subkey, plus a signing subkey when withSigningSubkey is
// set, which is the subkey gpg and the key-file signer then sign with.
func auditKey(t *testing.T, withSigningSubkey bool, cfg *packet.Config) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity("audit test", "", "audit@example.invalid", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if withSigningSubkey {
		if err := e.AddSigningSubkey(cfg); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func armored(t *testing.T, blockType string, fn func(io.Writer) error) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, blockType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := fn(w); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// privateKeyFile writes e's private key to a file, for --gpg-key.
func privateKeyFile(t *testing.T, e *openpgp.Entity) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "signing.asc")
	data := armored(t, openpgp.PrivateKeyType, func(w io.Writer) error { return e.SerializePrivateWithoutSigning(w, nil) })
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// publicKey is e's armored public key; withoutSigningSubkeys drops the
// signing subkeys, as an export made before one was added would.
func publicKey(t *testing.T, e *openpgp.Entity, withoutSigningSubkeys bool) []byte {
	t.Helper()
	c := *e
	if withoutSigningSubkeys {
		c.Subkeys = nil
		for _, sk := range e.Subkeys {
			if !sk.Sig.FlagSign {
				c.Subkeys = append(c.Subkeys, sk)
			}
		}
	}
	return armored(t, openpgp.PublicKeyType, c.Serialize)
}

// serveKey publishes body over https, returning its URL and a client that
// trusts the test server.
func serveKey(t *testing.T, status int, body []byte) (string, *http.Client) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/key.asc", srv.Client()
}

// signWithAudit signs a document both ways with the key file, under an audit
// against keyURL, and returns the audit's findings.
func signWithAudit(t *testing.T, keyFile, keyURL string, client *http.Client) (warnings []string, summary string) {
	t.Helper()
	a := NewAudit(keyFile, "", keyURL)
	a.HTTPClient = client
	s, err := NewKeyFileSigner(keyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	s = s.WithAudit(a)
	if _, err := s.SignDetached([]byte("<repomd/>")); err != nil {
		t.Fatalf("SignDetached: %v", err)
	}
	if _, err := s.SignClearsigned([]byte("Origin: test\n")); err != nil {
		t.Fatalf("SignClearsigned: %v", err)
	}
	return a.Warnings()
}

func containsAny(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestAuditPrimaryKeyVerifiedByPublishedKey(t *testing.T) {
	e := auditKey(t, false, nil)
	url, client := serveKey(t, http.StatusOK, publicKey(t, e, false))

	warnings, summary := signWithAudit(t, privateKeyFile(t, e), url, client)
	if len(warnings) != 0 {
		t.Errorf("warnings = %q, want none", warnings)
	}
	if !strings.Contains(summary, "2 signed item(s)") || !strings.Contains(summary, "verified against the public key at "+url) {
		t.Errorf("summary = %q", summary)
	}
}

func TestAuditWarnsAboutASubkeySignatureThePublishedKeyCovers(t *testing.T) {
	e := auditKey(t, true, nil)
	url, client := serveKey(t, http.StatusOK, publicKey(t, e, false))

	warnings, _ := signWithAudit(t, privateKeyFile(t, e), url, client)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %q, want exactly the subkey warning", warnings)
	}
	if !strings.Contains(warnings[0], "subkey") || !strings.Contains(warnings[0], "the public key at "+url+" does") {
		t.Errorf("warning = %q", warnings[0])
	}
}

// The case that motivated the audit: gpg signed with a subkey the published
// public key does not contain, so no user could verify the repository.
func TestAuditWarnsWhenThePublishedKeyLacksTheSigningSubkey(t *testing.T) {
	e := auditKey(t, true, nil)
	url, client := serveKey(t, http.StatusOK, publicKey(t, e, true))

	warnings, summary := signWithAudit(t, privateKeyFile(t, e), url, client)
	if !containsAny(warnings, "does not verify 2 signed item(s)") || !containsAny(warnings, "but not its subkey") {
		t.Errorf("warnings = %q, want the missing-subkey warning", warnings)
	}
	if !strings.Contains(summary, "does NOT verify") {
		t.Errorf("summary = %q", summary)
	}
}

func TestAuditWarnsWhenThePublishedKeyIsADifferentKey(t *testing.T) {
	e, other := auditKey(t, false, nil), auditKey(t, false, nil)
	url, client := serveKey(t, http.StatusOK, publicKey(t, other, false))

	warnings, _ := signWithAudit(t, privateKeyFile(t, e), url, client)
	if !containsAny(warnings, "it holds "+FingerprintOf(other)+", not the signing key "+FingerprintOf(e)) {
		t.Errorf("warnings = %q", warnings)
	}
}

func TestAuditRefusesASignatureByAnotherKey(t *testing.T) {
	e, other := auditKey(t, false, nil), auditKey(t, false, nil)
	s, err := NewKeyFileSigner(privateKeyFile(t, other), "")
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("<repomd/>")
	sig, err := s.SignDetached(data)
	if err != nil {
		t.Fatal(err)
	}

	a := NewAudit(privateKeyFile(t, e), "", "")
	err = a.Detached(data, sig)
	if err == nil || !strings.Contains(err.Error(), "neither the configured key "+FingerprintOf(e)) {
		t.Fatalf("Detached = %v, want a wrong-key error", err)
	}
	if w, summary := a.Warnings(); len(w) != 0 || summary != "" {
		t.Errorf("a refused signature was still recorded: %q, %q", w, summary)
	}
}

func TestAuditWithoutAKeyURL(t *testing.T) {
	e := auditKey(t, true, nil)
	warnings, summary := signWithAudit(t, privateKeyFile(t, e), "", nil)
	if !containsAny(warnings, "gpg --armor --export "+FingerprintOf(e)) {
		t.Errorf("warnings = %q, want the subkey warning to say how to export the key", warnings)
	}
	if !strings.Contains(summary, "no --gpg-key-url is recorded") {
		t.Errorf("summary = %q", summary)
	}
}

func TestAuditWarnsWhenTheKeyCannotBeFetched(t *testing.T) {
	e := auditKey(t, false, nil)
	url, client := serveKey(t, http.StatusNotFound, nil)
	warnings, _ := signWithAudit(t, privateKeyFile(t, e), url, client)
	if !containsAny(warnings, "could not fetch the public key at "+url) {
		t.Errorf("warnings = %q", warnings)
	}
}

func TestAuditWarnsAboutAPublishedPrivateKey(t *testing.T) {
	e := auditKey(t, false, nil)
	secret := armored(t, openpgp.PrivateKeyType, func(w io.Writer) error { return e.SerializePrivateWithoutSigning(w, nil) })
	url, client := serveKey(t, http.StatusOK, secret)
	warnings, _ := signWithAudit(t, privateKeyFile(t, e), url, client)
	if !containsAny(warnings, "PRIVATE key material") {
		t.Errorf("warnings = %q", warnings)
	}
}

func TestAuditWarnsAboutAKeyServedOverPlainHTTP(t *testing.T) {
	e := auditKey(t, false, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(publicKey(t, e, false))
	}))
	defer srv.Close()
	warnings, _ := signWithAudit(t, privateKeyFile(t, e), srv.URL+"/key.asc", nil)
	if !containsAny(warnings, "plain http") {
		t.Errorf("warnings = %q", warnings)
	}
}

func TestAuditWarnsAboutAKeyAboutToExpire(t *testing.T) {
	e := auditKey(t, true, &packet.Config{KeyLifetimeSecs: 10 * 24 * 3600})
	url, client := serveKey(t, http.StatusOK, publicKey(t, e, false))
	warnings, _ := signWithAudit(t, privateKeyFile(t, e), url, client)
	if !containsAny(warnings, "expires on") || !containsAny(warnings, "the public key at "+url+" says key") {
		t.Errorf("warnings = %q, want expiry warnings for both copies", warnings)
	}
}

func TestAuditReportsNothingWhenNothingWasSigned(t *testing.T) {
	var buf bytes.Buffer
	NewAudit("", "", "").Report(&buf)
	var nilAudit *Audit
	nilAudit.Report(&buf)
	if buf.Len() != 0 {
		t.Errorf("Report wrote %q", buf.String())
	}
}

func TestCheckKeyURL(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://example.com/RPM-GPG-KEY", true},
		{"http://example.com/key.asc", true},
		{"file:///etc/pki/rpm-gpg/key", false},
		{"ftp://example.com/key", false},
		{"https://", false},
		{"/key.asc", false},
		{"https://example.com/%zz", false},
	} {
		if err := CheckKeyURL(tc.url); (err == nil) != tc.ok {
			t.Errorf("CheckKeyURL(%q) = %v, want ok=%v", tc.url, err, tc.ok)
		}
	}
}
