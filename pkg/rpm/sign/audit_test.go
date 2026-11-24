package sign

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/danudey/createrepo-go/pkg/gpg"
)

// A package signed by a key's signing subkey, checked against a public key
// exported before that subkey existed: the audit must say users cannot verify
// it, and must stay quiet about verification once the full key is published.
func TestPackageSignAuditFindsAMissingSubkey(t *testing.T) {
	home, keyID, stalePub, _ := setupKey(t)
	t.Setenv("GNUPGHOME", home)
	env := append(os.Environ(), "GNUPGHOME="+home)
	// Loopback with an empty passphrase: without it gpg-agent opens pinentry to
	// ask for the new subkey's passphrase.
	run(t, env, "gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "", "--quick-add-key", keyID, "rsa2048", "sign", "0")
	fullPub := home + "/full.asc"
	writeCmd(t, env, fullPub, "gpg", "--export", "--armor", keyID)

	for _, tc := range []struct {
		name, pubFile string
		wantFailure   bool
	}{
		{"stale public key", stalePub, true},
		{"full public key", fullPub, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := os.ReadFile(tc.pubFile)
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
			defer srv.Close()

			a := gpg.NewAudit("", keyID, srv.URL+"/key.asc")
			a.HTTPClient = srv.Client()
			signer := NewPackageSignerKeyID(keyID, "")
			signer.Audit = a
			_, cleanup, err := signer.SignFile(referenceRPM)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()

			warnings, summary := a.Warnings()
			all := strings.Join(warnings, "\n")
			if !strings.Contains(all, "signed by subkey") {
				t.Errorf("no subkey warning in %q", all)
			}
			if got := strings.Contains(all, "but not its subkey"); got != tc.wantFailure {
				t.Errorf("missing-subkey warning = %v, want %v; warnings:\n%s", got, tc.wantFailure, all)
			}
			if !strings.HasPrefix(summary, "1 signed item(s)") {
				t.Errorf("summary = %q", summary)
			}
		})
	}
}
