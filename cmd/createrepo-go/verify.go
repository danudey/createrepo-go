package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/spf13/cobra"

	"github.com/danudey/createrepo-go/internal/cli"
	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/gpg"
	"github.com/danudey/createrepo-go/pkg/progress"
	"github.com/danudey/createrepo-go/pkg/rpm/pkgcheck"
	"github.com/danudey/createrepo-go/pkg/rpm/repo"
	"github.com/danudey/createrepo-go/pkg/rpm/repoconfig"
	"github.com/danudey/createrepo-go/pkg/rpm/sign"
)

// verifyFlags are verify's own flags.
type verifyFlags struct {
	checksums             bool
	concurrency           int
	requireTrusted        bool
	requireSignedPackages bool
	requireSignedMetadata bool
	skipPackageContents   bool
}

func verifyCmd() *cobra.Command {
	var vf verifyFlags
	cmd := &cobra.Command{
		Use:   "verify <repo-url>",
		Short: "Check that the published RPMs match the metadata and are intact and signed",
		Long: `Check that every RPM the metadata references is present with the size and
checksum the metadata records, that each RPM is intact and its signatures
sound, and that the repository's intra-repository dependencies are satisfied.
Nothing is modified.

How far the checksum check goes depends on the backend:

  local disk, SSH/SFTP  the checksum is computed from the stored file (over SSH
                        with sha256sum, so the RPM never crosses the network),
                        so contents are always proved.
  S3, GCS               the object store reports the checksum recorded when the
                        object was uploaded. That catches metadata and objects
                        disagreeing, but not an object whose content changed
                        afterwards.
  HTTP(S)               no checksum is available without downloading.

Pass --checksums to prove every RPM's checksum from its content regardless,
downloading each package the backend cannot hash in place. The summary always
says which of the three applied, so an unproved checksum is never reported as
though it had been verified.

Each RPM is also read and checked against itself: its header digests and every
signature it carries. Where the whole file is read anyway (local disk, or
--checksums against a backend that cannot hash in place) its payload digests
are checked too; elsewhere only the header is fetched, a few kilobytes per
package. A signature that does not match what it signs is always a problem.

Signatures are judged as the --target release's rpm judges them (the
repository's recorded target by default): RHEL 8 and 9 cannot read the rpm 6
OPENPGP signature, and RHEL 9 onwards refuses SHA-1 and DSA signatures.

Every package must be signed by the same key, and when the repository records
a signing key (createrepo-go.json), by that key. Trusted keys come from
--keyring, or failing that from the recorded key in the local GnuPG keyring;
without any, signatures are checked for integrity but not for who made them.

  --require-trusted          every signature must verify against a trusted key
  --require-signed-packages  every RPM must be signed (implied when the config
                             records sign_packages)
  --require-signed-metadata  repomd.xml must be signed (implied when the config
                             records sign_metadata)`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerify(cmd, args[0], vf)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&vf.checksums, "checksums", false, "prove every RPM's checksum from its content, downloading each one the backend cannot hash in place")
	f.IntVar(&vf.concurrency, "concurrency", 6, "number of packages to check in parallel")
	f.BoolVar(&vf.requireTrusted, "require-trusted", false, "fail unless every signature verifies against a trusted key (--keyring, or the repository's recorded key)")
	f.BoolVar(&vf.requireSignedPackages, "require-signed-packages", false, "fail unless every RPM carries a signature the target release accepts")
	f.BoolVar(&vf.requireSignedMetadata, "require-signed-metadata", false, "fail unless repomd.xml is signed (repomd.xml.asc)")
	f.BoolVar(&vf.skipPackageContents, "skip-package-contents", false, "do not read the RPMs: skip their digest and signature checks")
	return cmd
}

func runVerify(cmd *cobra.Command, location string, vf verifyFlags) error {
	ctx := cli.Ctx(cmd)
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	be, err := backend.Open(ctx, location)
	if err != nil {
		return err
	}
	cfg, err := repoconfig.Load(ctx, be)
	if err != nil {
		closeBackend(be)
		return err
	}
	// verify only reads, so the recorded config contributes what to expect —
	// target, key, signing — and never a signer.
	opts, err := repoOptions(false)
	if err != nil {
		closeBackend(be)
		return err
	}
	r, err := repo.OpenWith(ctx, be, opts)
	if err != nil {
		return err
	}
	defer r.Close()

	vopt, err := verifyOptions(cmd, cfg, vf)
	if err != nil {
		return err
	}
	if vf.checksums {
		vopt.Checksums = repo.ChecksumContent
		// A full content check can be a large transfer; say so before
		// starting rather than appearing to hang.
		if n, size := r.DownloadEstimate(); n > 0 {
			fmt.Fprintf(out, "hashing %d package(s) from %s: ~%s to download\n", n, r.Backend(), progress.Bytes(size))
		}
	}

	res, err := r.Verify(ctx, vopt)
	if err != nil {
		return err
	}
	for _, p := range res.Warnings {
		fmt.Fprintln(errOut, "WARNING:", p)
	}
	for _, p := range res.Problems {
		fmt.Fprintln(errOut, "PROBLEM:", p)
	}
	if !res.OK() {
		return fmt.Errorf("%d problem(s) found", len(res.Problems))
	}
	fmt.Fprintln(out, verifySummary(res, !vf.skipPackageContents))
	return nil
}

// verifyOptions resolves what verify expects of the repository: the target
// whose rpm judges the signatures, the trusted keys, the key the repository
// records, and which signatures are required.
func verifyOptions(cmd *cobra.Command, cfg *repoconfig.Config, vf verifyFlags) (repo.VerifyOptions, error) {
	errOut := cmd.ErrOrStderr()
	opt := repo.VerifyOptions{
		Checksums:             repo.ChecksumCheap,
		Concurrency:           vf.concurrency,
		RequireTrusted:        vf.requireTrusted,
		RequireSignedPackages: vf.requireSignedPackages,
		RequireSignedMetadata: vf.requireSignedMetadata,
		SkipPackageContents:   vf.skipPackageContents,
	}

	target := gf.target
	if !cmd.Flags().Changed("target") && cfg != nil {
		target = cfg.Target
	}
	pol, err := pkgcheck.PolicyFor(canonicalTarget(target))
	if err != nil {
		return opt, err
	}
	opt.Policy = pol

	var recordedKey string
	if cfg != nil {
		recordedKey = cfg.GPGKeyID
		opt.RequireSignedPackages = opt.RequireSignedPackages || cfg.SignPackages
		opt.RequireSignedMetadata = opt.RequireSignedMetadata || cfg.SignMetadata
	}
	kr, err := trustedKeys(errOut, recordedKey)
	if err != nil {
		return opt, err
	}
	opt.Keyring = kr
	if recordedKey != "" {
		opt.ExpectedKeyIDs, opt.ExpectedKeyIncomplete = expectedKeyIDs(recordedKey, kr)
	}
	if opt.RequireTrusted && len(kr) == 0 {
		return opt, fmt.Errorf("--require-trusted needs trusted keys: pass --keyring, or verify a repository whose %s records a signing key that is in the local GnuPG keyring", repoconfig.Path)
	}
	return opt, nil
}

// expectedKeyIDs expands the recorded signing key into the long key ids that
// may sign on its behalf — its own and its subkeys' — using the trusted keys
// when they hold it, and the local GnuPG keyring otherwise. incomplete is true
// when the key was found in neither, so its subkeys are unknown.
func expectedKeyIDs(recorded string, kr openpgp.EntityList) (ids []string, incomplete bool) {
	want := strings.ToLower(strings.ReplaceAll(recorded, " ", ""))
	for _, e := range kr {
		if !strings.HasSuffix(strings.ToLower(gpg.FingerprintOf(e)), want) {
			continue
		}
		ids = append(ids, e.PrimaryKey.KeyIdString())
		for _, sk := range e.Subkeys {
			ids = append(ids, sk.PublicKey.KeyIdString())
		}
		return ids, false
	}
	// Not among the trusted keys; the local keyring may still know it.
	_, cleanup, err := sign.ExportPublicKey(recorded)
	cleanup()
	return sign.KeyAndSubkeyIDs(recorded), err != nil
}

// trustedKeys loads the keys signatures are verified against: --keyring, or
// failing that the repository's recorded key from the local GnuPG keyring. No
// keys is not an error; signatures are then checked for integrity only.
func trustedKeys(errOut io.Writer, recordedKey string) (openpgp.EntityList, error) {
	if len(gf.keyrings) > 0 {
		return gpg.LoadKeyring(gf.keyrings...)
	}
	if recordedKey == "" {
		return nil, nil
	}
	path, cleanup, err := sign.ExportPublicKey(recordedKey)
	if err != nil {
		fmt.Fprintf(errOut, "warning: the repository's signing key %s is not in the local GnuPG keyring (%v); signatures are checked for integrity and signer, but not verified. Pass --keyring to supply it.\n", recordedKey, err)
		return nil, nil
	}
	defer cleanup()
	return gpg.LoadKeyring(path)
}

// verifySummary renders a clean verification result, stating exactly what was
// established — how each checksum was confirmed, how much of each package was
// read, and who signed what — so the reader is never left assuming more was
// proved than actually was.
func verifySummary(res *repo.VerifyResult, readPackages bool) string {
	var parts []string
	if res.ChecksumVerified > 0 {
		parts = append(parts, fmt.Sprintf("%d verified from content", res.ChecksumVerified))
	}
	if res.ChecksumRecorded > 0 {
		parts = append(parts, fmt.Sprintf("%d matched the store's recorded checksum", res.ChecksumRecorded))
	}
	if res.ChecksumUnconfirmed > 0 {
		parts = append(parts, fmt.Sprintf("%d not confirmed", res.ChecksumUnconfirmed))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "OK: %d package(s) present with the expected size", res.Packages)
	if len(parts) > 0 {
		b.WriteString("; checksums: " + strings.Join(parts, ", "))
	}
	b.WriteString("; dependencies satisfied")

	if readPackages {
		fmt.Fprintf(&b, "\npackages: %d intact", res.HeadersChecked)
		switch headerOnly := res.HeadersChecked - res.PayloadsChecked; {
		case headerOnly == 0:
			b.WriteString(" (header and payload digests checked)")
		case res.PayloadsChecked == 0:
			b.WriteString(" (header digests checked; payloads not read)")
		default:
			fmt.Fprintf(&b, " (%d with payload digests checked, %d header only)", res.PayloadsChecked, headerOnly)
		}
		b.WriteString("; signatures: " + signerSummary(res))
	}
	b.WriteString("\nrepomd.xml: " + metadataSummary(res.Metadata))

	if res.ChecksumUnconfirmed > 0 || (readPackages && res.PayloadsChecked < res.HeadersChecked) {
		b.WriteString("\nRun with --checksums to download the packages this backend cannot hash in place, proving their checksums and payload digests.")
	}
	return b.String()
}

func signerSummary(res *repo.VerifyResult) string {
	if len(res.Signers) == 0 {
		return "none"
	}
	ids := make([]string, 0, len(res.Signers))
	for id := range res.Signers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var parts []string
	for _, id := range ids {
		trust := "not verified: no trusted key"
		if res.Trusted[id] {
			trust = "verified against a trusted key"
		}
		parts = append(parts, fmt.Sprintf("%d signed by %s (%s)", res.Signers[id], id, trust))
	}
	if res.Unsigned > 0 {
		parts = append(parts, fmt.Sprintf("%d unsigned", res.Unsigned))
	}
	return strings.Join(parts, ", ")
}

func metadataSummary(m repo.MetadataSignature) string {
	if !m.Present {
		return "not signed"
	}
	var parts []string
	for _, s := range m.Signatures {
		trust := "not verified: no trusted key"
		if s.Status == pkgcheck.StatusGood {
			trust = "verified against a trusted key"
		}
		parts = append(parts, fmt.Sprintf("signed by %s (%s)", s.Signer(), trust))
	}
	return strings.Join(parts, ", ")
}
