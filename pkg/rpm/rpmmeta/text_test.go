package rpmmeta

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withSummary writes a copy of the reference package whose summary starts
// with replacement instead of "A friendly" (the same length, so the header
// layout is undisturbed). The edit breaks the header digest, which the tests
// that use it do not ask to be checked.
func withSummary(t *testing.T, replacement string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(referenceRepo, "hello-2.10-3.noarch.rpm"))
	if err != nil {
		t.Fatal(err)
	}
	old := []byte("A friendly")
	if len(replacement) != len(old) {
		t.Fatalf("replacement %q must be %d bytes", replacement, len(old))
	}
	i := bytes.Index(data, old)
	copy(data[i:], replacement)
	path := filepath.Join(t.TempDir(), "hello.rpm")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInvalidUTF8IsReadAsLatin1LikeCreaterepoC(t *testing.T) {
	p, err := FromFile(withSummary(t, "A fri\xe9ndly"), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.Summary, "A friéndly") {
		t.Errorf("summary = %q; want the Latin-1 byte converted to é", p.Summary)
	}
}

func TestControlCharactersAreRefusedLikeCreaterepoC(t *testing.T) {
	for _, s := range []string{"A\x01\x02riendly", "A\x1b[2Jendly"} {
		_, err := FromFile(withSummary(t, s), DefaultOptions())
		if err == nil || !strings.Contains(err.Error(), "summary contains the control character") {
			t.Errorf("summary %q: err = %v; want a refusal naming the field", s, err)
		}
	}
}

func TestTheIntegrityCheckRefusesADamagedPackage(t *testing.T) {
	path := withSummary(t, "B friendly")
	if _, err := FromFile(path, DefaultOptions()); err != nil {
		t.Fatalf("without the check the package reads: %v", err)
	}
	opt := DefaultOptions()
	opt.CheckIntegrity = true
	if _, err := FromFile(path, opt); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Errorf("err = %v; want the damaged header refused", err)
	}
	if _, err := FromFile(filepath.Join(referenceRepo, "hello-2.10-3.noarch.rpm"), opt); err != nil {
		t.Errorf("an intact package was refused: %v", err)
	}
}
