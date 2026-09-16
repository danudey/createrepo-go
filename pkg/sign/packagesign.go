package sign

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SignatureFormat selects the rpm signature header layout written when signing.
type SignatureFormat string

const (
	// SigV4 writes the legacy v4 header signature (the RSAHEADER tag). This is
	// the format rpm < 6 reads, so it is required for RHEL 8 and RHEL 9 (rpm
	// 4.14 and 4.16 cannot read the newer OPENPGP tag). On an rpm 6 host this
	// is requested with rpmsign --rpmv4, which adds the legacy signature
	// alongside the default OPENPGP one, yielding a package installable on
	// RHEL 8/9/10 alike.
	SigV4 SignatureFormat = "v4"
	// SigOpenPGP writes only the rpm 6 OPENPGP header signature. It is read by
	// rpm 6+ (RHEL 10, recent Fedora) but NOT by RHEL 8/9.
	SigOpenPGP SignatureFormat = "openpgp"
)

// PackageSigner signs RPM packages by delegating to rpmsign/gpg. It can use a
// key identifier from the user's keyring or import a private key file into a
// throwaway GnuPG home. Signing is always performed on a copy so the caller's
// original file is left untouched.
type PackageSigner struct {
	// Format selects the signature header layout (default SigV4, the most
	// broadly compatible choice).
	Format SignatureFormat

	// Replace strips any existing signatures before adding the new one, so the
	// result is signed only by this signer's key. It is used when re-signing an
	// already-signed package with a different key (rebuild --resign-packages);
	// without it rpmsign would leave the previous key's signature in place.
	Replace bool

	keyID      string
	gpgHome    string // throwaway GNUPGHOME when signing from a key file
	passphrase string
	cleanup    func()
}

// NewPackageSignerKeyID returns a signer that uses an existing keyring key.
func NewPackageSignerKeyID(keyID, passphrase string) *PackageSigner {
	return &PackageSigner{keyID: keyID, passphrase: passphrase, cleanup: func() {}}
}

// NewPackageSignerKeyFile imports the private key file into a temporary GnuPG
// home and returns a signer bound to it. Call Close when finished to remove the
// temporary home.
func NewPackageSignerKeyFile(keyFile, passphrase string) (*PackageSigner, error) {
	home, err := os.MkdirTemp("", "cr-gpg-*")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(home, 0o700); err != nil {
		os.RemoveAll(home)
		return nil, err
	}
	cleanup := func() { os.RemoveAll(home) }

	imp := exec.Command(gpgBinary(), "--homedir", home, "--batch", "--yes", "--import", keyFile)
	if out, err := imp.CombinedOutput(); err != nil {
		cleanup()
		return nil, fmt.Errorf("import key: %v: %s", err, out)
	}
	fpr, err := firstSecretKeyFingerprint(home)
	if err != nil {
		cleanup()
		return nil, err
	}
	return &PackageSigner{keyID: fpr, gpgHome: home, passphrase: passphrase, cleanup: cleanup}, nil
}

// Close removes any temporary GnuPG home.
func (s *PackageSigner) Close() {
	if s.cleanup != nil {
		s.cleanup()
	}
}

// SignFile copies src to a temporary file, signs the copy with rpmsign, and
// returns the path of the signed copy plus a cleanup function.
func (s *PackageSigner) SignFile(src string) (string, func(), error) {
	tmpDir, err := os.MkdirTemp("", "cr-sign-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmpDir) }
	dst := filepath.Join(tmpDir, filepath.Base(src))
	if err := copyFile(src, dst); err != nil {
		cleanup()
		return "", nil, err
	}

	// When replacing, remove any existing signatures first so the package ends
	// up signed only by the new key (rpmsign --addsign otherwise leaves a prior
	// key's header signature alongside the new one).
	if s.Replace {
		del := exec.Command("rpmsign", "--delsign", dst)
		if s.gpgHome != "" {
			del.Env = append(os.Environ(), "GNUPGHOME="+s.gpgHome)
		}
		if out, err := del.CombinedOutput(); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("rpmsign --delsign %s: %v: %s", src, err, out)
		}
	}

	args := []string{"--addsign", "--define", "_gpg_name " + s.keyID}
	// For the v4 (legacy) format, ensure the RSAHEADER signature that RHEL 8/9
	// read is present. rpm 6 writes only the new OPENPGP tag by default, so we
	// request --rpmv4 to additionally emit the legacy signature; the flag does
	// not exist on rpm 4.x (which already writes the compatible signature), so
	// it is added only when the local rpmsign advertises it. For the openpgp
	// format we leave the host default in place.
	if s.Format != SigOpenPGP && rpmsignSupportsRPMV4() {
		args = append(args, "--rpmv4")
	}
	if s.gpgHome != "" {
		args = append(args, "--define", "_gpg_path "+s.gpgHome)
	}
	if s.passphrase != "" {
		// Use a loopback pinentry so gpg reads the passphrase non-interactively.
		signCmd := fmt.Sprintf(
			"%%{__gpg} gpg --batch --no-verbose --no-armor --pinentry-mode loopback --passphrase %s -u \"%%{_gpg_name}\" -sbo %%{__signature_filename} --digest-algo sha256 %%{__plaintext_filename}",
			s.passphrase)
		args = append(args, "--define", "__gpg_sign_cmd "+signCmd)
	}
	args = append(args, dst)

	cmd := exec.Command("rpmsign", args...)
	if s.gpgHome != "" {
		cmd.Env = append(os.Environ(), "GNUPGHOME="+s.gpgHome)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("rpmsign %s: %v: %s", src, err, out)
	}
	return dst, cleanup, nil
}

// rpmsignSupportsRPMV4 reports whether the local rpmsign understands the
// --rpmv4 compatibility flag (rpm 6+).
func rpmsignSupportsRPMV4() bool {
	out, _ := exec.Command("rpmsign", "--help").CombinedOutput()
	return strings.Contains(string(out), "--rpmv4")
}

func firstSecretKeyFingerprint(home string) (string, error) {
	cmd := exec.Command(gpgBinary(), "--homedir", home, "--batch", "--with-colons", "--list-secret-keys")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("list imported keys: %w", err)
	}
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) > 9 && fields[0] == "fpr" {
			return fields[9], nil
		}
	}
	return "", fmt.Errorf("no secret key fingerprint found after import")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
