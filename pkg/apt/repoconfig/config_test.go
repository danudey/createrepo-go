package repoconfig

import (
	"context"
	"testing"

	"github.com/danudey/createrepo-go/pkg/backend"
)

// The config is read from repositories this tool does not control, so a key id
// that gpg would parse as an option must be refused on load. A fingerprint
// pinned with gpg's trailing "!" is accepted.
func TestLoadChecksTheKeyID(t *testing.T) {
	for keyID, ok := range map[string]bool{
		"ABCDEF0123456789ABCDEF0123456789ABCDEF01!": true,
		"0xABCDEF0123456789":                        true,
		"--output=/home/me/.bashrc":                 false,
		"CR Test <cr-test@example.com>":             false,
	} {
		be, err := backend.Open(context.Background(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := Save(context.Background(), be, &Config{SignRelease: true, GPGKeyID: keyID}); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(context.Background(), be)
		if ok && (err != nil || cfg.GPGKeyID != keyID) {
			t.Errorf("Load with gpg_key_id %q = (%+v, %v), want it accepted", keyID, cfg, err)
		}
		if !ok && err == nil {
			t.Errorf("Load accepted gpg_key_id %q", keyID)
		}
	}
}
