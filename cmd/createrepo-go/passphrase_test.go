package main

import (
	"bytes"
	"strings"
	"testing"
)

const testPassphrase = "hunter2-secret"

// The passphrase may come from $CREATEREPO_GPG_PASSPHRASE, but --help must not print it, as it
// would if the variable were the flag's default.
func TestHelpDoesNotPrintThePassphrase(t *testing.T) {
	t.Setenv("CREATEREPO_GPG_PASSPHRASE", testPassphrase)
	root := rootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), testPassphrase) {
		t.Errorf("--help printed the passphrase:\n%s", buf.String())
	}
}

// $CREATEREPO_GPG_PASSPHRASE still supplies the passphrase, and --gpg-passphrase overrides it.
func TestPassphraseComesFromTheEnvironment(t *testing.T) {
	t.Setenv("CREATEREPO_GPG_PASSPHRASE", testPassphrase)
	for _, tc := range []struct {
		flags []string
		want  string
	}{
		{nil, testPassphrase},
		{[]string{"--gpg-passphrase", "from-the-flag"}, "from-the-flag"},
	} {
		root := rootCmd()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(append([]string{"create", t.TempDir(), "--dry-run"}, tc.flags...))
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		if gf.gpgPass != tc.want {
			t.Errorf("with flags %q the passphrase is %q, want %q", tc.flags, gf.gpgPass, tc.want)
		}
	}
}
