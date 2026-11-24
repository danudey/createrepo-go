package sign

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestFingerprint(t *testing.T) {
	home, keyID, _, secFile := setupKey(t)
	t.Setenv("GNUPGHOME", home)

	// setupKey's keyID is already the primary-key fingerprint.
	want := keyID

	t.Run("from key file", func(t *testing.T) {
		got, err := Fingerprint(secFile, "")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("key-file fingerprint = %q, want %q", got, want)
		}
	})

	t.Run("from keyring id", func(t *testing.T) {
		got, err := Fingerprint("", keyID)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("keyring fingerprint = %q, want %q", got, want)
		}
	})

	t.Run("from keyring uid", func(t *testing.T) {
		got, err := Fingerprint("", "cr-test@example.com")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("uid fingerprint = %q, want %q", got, want)
		}
	})

	// A trailing "!" pins a key: it resolves to that key's own fingerprint
	// with the "!" kept, so the pin survives being recorded.
	t.Run("pinned subkey", func(t *testing.T) {
		env := append(os.Environ(), "GNUPGHOME="+home)
		run(t, env, "gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "",
			"--quick-add-key", keyID, "rsa2048", "sign", "0")
		var sub string
		out, err := exec.Command("gpg", "--with-colons", "--list-keys", keyID).Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if f := strings.Split(line, ":"); len(f) > 9 && f[0] == "fpr" && f[9] != keyID {
				sub = f[9]
			}
		}
		if sub == "" {
			t.Fatal("no subkey fingerprint listed")
		}
		for _, id := range []string{sub + "!", sub[len(sub)-16:] + "!", strings.ToLower(sub[len(sub)-16:]) + "!"} {
			if got, err := Fingerprint("", id); err != nil || got != sub+"!" {
				t.Errorf("Fingerprint(%q) = %q, %v; want %q", id, got, err, sub+"!")
			}
		}
		if got, err := Fingerprint("", keyID+"!"); err != nil || got != keyID+"!" {
			t.Errorf("pinned primary = %q, %v; want %q", got, err, keyID+"!")
		}
		// Unpinned, the subkey still resolves to its primary key.
		if got, err := Fingerprint("", sub); err != nil || got != want {
			t.Errorf("unpinned subkey = %q, %v; want %q", got, err, want)
		}
	})

	t.Run("neither set", func(t *testing.T) {
		got, err := Fingerprint("", "")
		if err != nil || got != "" {
			t.Errorf("expected empty result, got %q, %v", got, err)
		}
	})
}
