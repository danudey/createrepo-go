package sign

import "testing"

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

	t.Run("neither set", func(t *testing.T) {
		got, err := Fingerprint("", "")
		if err != nil || got != "" {
			t.Errorf("expected empty result, got %q, %v", got, err)
		}
	})
}
