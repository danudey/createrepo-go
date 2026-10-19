// Package gpg holds the OpenPGP work both tools share: signing a repository's
// top-level index (repomd.xml, or an apt Release), verifying such a signature
// against trusted keys, and resolving and exporting keys.
//
// Two key sources are supported: a private key file (handled natively with
// OpenPGP, so no gpg is needed) and a key identifier from the user's GnuPG
// keyring (delegated to the gpg binary). Only the second can reach a passphrase
// prompt; see NoPassphrasePrompt.
package gpg

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
)

// Signer signs a document in either of the forms a repository publishes: a
// detached armored signature (repomd.xml.asc, an apt Release.gpg) or an inline
// clearsigned document (an apt InRelease).
type Signer struct {
	// entity is set for key-file signing (native).
	entity *openpgp.Entity
	// keyID is set for keyring signing (delegated to gpg).
	keyID string
	// passphrase unlocks the key when gpg needs one supplied non-interactively.
	passphrase string
}

// NewKeyFileSigner loads an OpenPGP private key from path (armored or binary)
// and returns a signer. If the key is passphrase-protected, passphrase is used
// to decrypt it.
func NewKeyFileSigner(path, passphrase string) (*Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	entities, err := ReadKeyRing(data)
	if err != nil {
		return nil, fmt.Errorf("read signing key %s: %w", path, err)
	}
	var signer *openpgp.Entity
	for _, e := range entities {
		if e.PrivateKey != nil {
			signer = e
			break
		}
	}
	if signer == nil {
		return nil, fmt.Errorf("no private key found in %s", path)
	}
	if signer.PrivateKey.Encrypted {
		if passphrase == "" {
			return nil, fmt.Errorf("signing key %s is encrypted but no passphrase was provided", path)
		}
		if err := decryptEntity(signer, []byte(passphrase)); err != nil {
			return nil, fmt.Errorf("decrypt signing key: %w", err)
		}
	}
	return &Signer{entity: signer}, nil
}

// NewKeyIDSigner returns a signer that delegates to gpg, using the given key id
// (or any gpg-recognized user-id/fingerprint) from the local keyring.
func NewKeyIDSigner(keyID, passphrase string) *Signer {
	return &Signer{keyID: keyID, passphrase: passphrase}
}

// SignDetached returns an ASCII-armored detached signature over data.
func (s *Signer) SignDetached(data []byte) ([]byte, error) {
	if s.entity != nil {
		var buf bytes.Buffer
		if err := openpgp.ArmoredDetachSign(&buf, s.entity, bytes.NewReader(data), nil); err != nil {
			return nil, err
		}
		// apt requires the armored signature to end in a newline, and nothing
		// else minds one.
		return ensureTrailingNewline(buf.Bytes()), nil
	}
	return s.gpg(data, "--armor", "--detach-sign")
}

// SignClearsigned wraps data in an OpenPGP clearsigned document.
func (s *Signer) SignClearsigned(data []byte) ([]byte, error) {
	if s.entity != nil {
		// Sign with the key a detached signature would use: the signing
		// subkey when there is one. The primary key of a production key is
		// often certify-only, and a signature from it is one gpgv refuses,
		// so InRelease (the file apt prefers) would fail where Release.gpg
		// passed.
		key, ok := s.entity.SigningKey(time.Now())
		if !ok {
			return nil, fmt.Errorf("the signing key has no key usable for signing")
		}
		var buf bytes.Buffer
		w, err := clearsign.Encode(&buf, key.PrivateKey, nil)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return ensureTrailingNewline(buf.Bytes()), nil
	}
	return s.gpg(data, "--clearsign")
}

// gpg shells out to the gpg binary to sign data with the configured key.
func (s *Signer) gpg(data []byte, mode ...string) ([]byte, error) {
	args := append([]string{"--yes", "--output", "-"}, mode...)
	if s.keyID != "" {
		args = append(args, "--local-user", s.keyID)
	}
	switch {
	case s.passphrase != "":
		// --pinentry-mode loopback is what lets a passphrase be supplied
		// without a terminal, which is the case in any automated publish.
		args = append(args, "--pinentry-mode", "loopback", "--passphrase-fd", "0")
	case NoPassphrasePrompt:
		// No passphrase to hand over and none may be asked for: "error" makes
		// gpg-agent fail the operation instead of launching pinentry.
		args = append(args, "--pinentry-mode", "error")
	}

	cmd := Command(args...)
	if s.passphrase != "" {
		cmd.Stdin = strings.NewReader(s.passphrase + "\n")
		// The document to sign cannot also come from stdin, so it is passed by
		// file instead.
		tmp, cleanup, err := writeTemp(data)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		cmd.Args = append(cmd.Args, tmp)
	} else {
		cmd.Stdin = bytes.NewReader(data)
	}

	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		hint := ""
		if s.passphrase == "" && NoPassphrasePrompt {
			hint = " (prompting is disabled: if the key is passphrase-protected," +
				" supply it with --gpg-passphrase)"
		}
		return nil, fmt.Errorf("gpg %s: %w: %s%s", strings.Join(mode, " "), err, errBuf.String(), hint)
	}
	return ensureTrailingNewline(out.Bytes()), nil
}

// writeTemp spills data to a temporary file, returning its path and a cleanup.
func writeTemp(data []byte) (string, func(), error) {
	f, err := os.CreateTemp("", "gpg-sign-*")
	if err != nil {
		return "", func() {}, err
	}
	name := f.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return name, cleanup, nil
}

// NoPassphrasePrompt stops any gpg invocation from asking a human for a key
// passphrase, failing the operation instead.
//
// --batch alone is not enough. It keeps gpg itself from asking questions, but
// the passphrase is asked for by gpg-agent, which launches pinentry on its own
// and will happily use a terminal or an X display if it finds one. On a CI
// runner that allocates a TTY, that turns a missing passphrase into a build
// that hangs on a prompt nobody is there to answer, until the job times out.
var NoPassphrasePrompt bool

// Command builds a gpg invocation. Every call is --batch, and when
// NoPassphrasePrompt is set the child is additionally denied the two things
// pinentry needs to reach a human: a controlling terminal and a display.
func Command(args ...string) *exec.Cmd {
	prefix := []string{"--batch"}
	if NoPassphrasePrompt {
		prefix = append(prefix, "--no-tty")
	}
	cmd := exec.Command(Binary(), append(prefix, args...)...)
	if NoPassphrasePrompt {
		cmd.Env = envWithout(promptVars...)
	}
	return cmd
}

// PromptlessEnv returns the current environment without the variables
// pinentry uses to reach a human: a terminal and a display. It is for a gpg run
// by another program (rpmsign) when NoPassphrasePrompt is set.
func PromptlessEnv() []string { return envWithout(promptVars...) }

// promptVars are what pinentry looks for to put a prompt in front of someone.
var promptVars = []string{"GPG_TTY", "DISPLAY", "WAYLAND_DISPLAY"}

// envWithout returns the current environment with the named variables dropped.
func envWithout(names ...string) []string {
	drop := make(map[string]bool, len(names))
	for _, n := range names {
		drop[n] = true
	}
	env := os.Environ()
	kept := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !drop[name] {
			kept = append(kept, kv)
		}
	}
	return kept
}

// binaryEnvs name the variables that override the gpg command, one per tool;
// the first one set wins.
var binaryEnvs = []string{"CREATEREPO_GPG", "CREATEAPT_GPG"}

// Binary returns the gpg command to invoke: $CREATEREPO_GPG or $CREATEAPT_GPG
// when set, for testing and for hosts where gpg is not on the default path, and
// "gpg" otherwise.
func Binary() string {
	for _, env := range binaryEnvs {
		if g := os.Getenv(env); g != "" {
			return g
		}
	}
	return "gpg"
}

func ensureTrailingNewline(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] != '\n' {
		return append(b, '\n')
	}
	return b
}

// ReadKeyRing reads OpenPGP entities, accepting either armored or binary input.
func ReadKeyRing(data []byte) (openpgp.EntityList, error) {
	if block, err := armor.Decode(bytes.NewReader(data)); err == nil {
		return openpgp.ReadKeyRing(block.Body)
	}
	return openpgp.ReadKeyRing(bytes.NewReader(data))
}

// decryptEntity decrypts a private key and its subkeys in place.
func decryptEntity(e *openpgp.Entity, passphrase []byte) error {
	if e.PrivateKey != nil && e.PrivateKey.Encrypted {
		if err := e.PrivateKey.Decrypt(passphrase); err != nil {
			return err
		}
	}
	for _, sk := range e.Subkeys {
		if sk.PrivateKey != nil && sk.PrivateKey.Encrypted {
			if err := sk.PrivateKey.Decrypt(passphrase); err != nil {
				return err
			}
		}
	}
	return nil
}
