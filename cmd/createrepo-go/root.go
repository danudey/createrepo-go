package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danudey/createrepo-go/pkg/repo"
	"github.com/danudey/createrepo-go/pkg/sign"
)

// version is overridable at build time with -ldflags.
var version = "dev"

// globalFlags holds options shared across subcommands.
type globalFlags struct {
	target         string
	checksum       string
	compression    string
	locationPrefix string
	changelogLimit int
	dryRun         bool
	force          bool

	// pruneBreakDeps permits add/rebuild --prune-older to drop a version even
	// when another package still depends on that specific version.
	pruneBreakDeps bool

	// rebuild-only directory-scan cleanups.
	removeUnreferencedRPMs bool
	removeStaleMetadata    bool

	// AWS/S3 credentials selection. These map onto the AWS_PROFILE / AWS_REGION
	// environment variables, which the AWS SDK's default config loader honours.
	awsProfile string
	awsRegion  string

	// signing / verification
	signMetadata    bool
	signPackages    bool
	signatureFormat string
	verifySigs      bool
	gpgKey          string
	gpgKeyID        string
	gpgPass         string
	keyrings        []string
}

// compatProfile is the set of defaults a --target selects.
type compatProfile struct {
	compression string
	signature   string
}

// profiles maps a release target to its recommended defaults. RHEL 8 and 9
// both require the legacy v4 (RSAHEADER) package signature because their rpm
// (4.14 / 4.16) cannot read the rpm 6 OPENPGP tag; only RHEL 10 (rpm 6) uses
// OPENPGP. gzip is used for RHEL 8 since zstd metadata needs RHEL 8.4+.
var profiles = map[string]compatProfile{
	"rhel8":  {compression: "gzip", signature: sigFormatV4},
	"rhel9":  {compression: "zstd", signature: sigFormatV4},
	"rhel10": {compression: "zstd", signature: sigFormatOpenPGP},
}

// profileAliases maps friendly names to canonical profile keys.
var profileAliases = map[string]string{
	"el8": "rhel8", "rocky8": "rhel8", "alma8": "rhel8", "almalinux8": "rhel8", "centos8": "rhel8",
	"el9": "rhel9", "rocky9": "rhel9", "alma9": "rhel9", "almalinux9": "rhel9", "centos9": "rhel9",
	"el10": "rhel10", "rocky10": "rhel10", "alma10": "rhel10", "almalinux10": "rhel10", "centos10": "rhel10",
}

var gf globalFlags

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "createrepo-go",
		Short: "Lightweight dnf/yum (rpm-md) repository manager",
		Long: `createrepo-go creates and maintains dnf/yum (rpm-md) repositories on local
disk or remote storage (SSH/SFTP, S3, GCS; HTTP(S) read-only).

It updates a repository in place, transferring as little data as possible:
only the (small) metadata is fetched and rewritten, RPMs already present are
validated remotely by checksum rather than re-uploaded, and existing RPMs are
never downloaded.`,
		SilenceUsage:      true,
		SilenceErrors:     true,
		Version:           version,
		PersistentPreRunE: preRunE,
	}

	pf := root.PersistentFlags()
	pf.StringVar(&gf.target, "target", "", "compatibility profile setting defaults: rhel8, rhel9, rhel10 (aliases: el8/alma9/...)")
	pf.StringVar(&gf.checksum, "checksum", "sha256", "metadata/package checksum algorithm")
	pf.StringVar(&gf.compression, "compression", "gzip", "metadata compression: gzip or zstd (zstd needs RHEL 8.4+)")
	pf.StringVar(&gf.signatureFormat, "signature-format", sigFormatV4, "package signature format: v4 (RSAHEADER; RHEL 8/9) or openpgp (rpm 6; RHEL 10+)")
	pf.StringVar(&gf.locationPrefix, "location-prefix", "Packages", "subdirectory under the repo root for uploaded RPMs (empty string to upload at the repo root)")
	pf.StringVar(&gf.awsProfile, "profile", "", "AWS named profile for S3 access (sets AWS_PROFILE; MFA-protected assume-role profiles are prompted for on stdin)")
	pf.StringVar(&gf.awsRegion, "region", "", "AWS region for S3 access (sets AWS_REGION); the bucket's actual region is detected and used if it differs")
	pf.IntVar(&gf.changelogLimit, "changelog-limit", 10, "most-recent changelog entries to keep per package (0 = all)")
	pf.BoolVar(&gf.dryRun, "dry-run", false, "show what would change without transferring anything")
	pf.BoolVar(&gf.force, "force", false, "overwrite a remote file whose content differs from the local RPM")

	pf.BoolVar(&gf.signMetadata, "sign-metadata", false, "GPG-sign repomd.xml (writes repomd.xml.asc)")
	pf.BoolVar(&gf.signPackages, "sign-packages", false, "GPG-sign RPMs (rpmsign) before uploading")
	pf.BoolVar(&gf.verifySigs, "verify-sigs", false, "verify each RPM's signature against --keyring before adding")
	pf.StringVar(&gf.gpgKey, "gpg-key", "", "path to a GPG private key file (for signing)")
	pf.StringVar(&gf.gpgKeyID, "gpg-key-id", "", "GPG key id/uid from the local keyring (for signing)")
	pf.StringVar(&gf.gpgPass, "gpg-passphrase", os.Getenv("CREATEREPO_GPG_PASSPHRASE"), "passphrase for the signing key (or set CREATEREPO_GPG_PASSPHRASE)")
	pf.StringArrayVar(&gf.keyrings, "keyring", nil, "public keyring file for --verify-sigs (repeatable)")

	root.AddCommand(addCmd(), removeCmd(), rebuildCmd(), copyCmd(), createCmd(), listCmd(), verifyCmd(), checkCmd())
	return root
}

// preRunE runs before every subcommand: it resolves the compatibility profile
// and validates that signing-related flags are coherent.
func preRunE(cmd *cobra.Command, args []string) error {
	if err := applyProfile(cmd, args); err != nil {
		return err
	}
	applyAWSEnv()
	// rebuild --resign-packages is an explicit instruction to sign the packages,
	// so it satisfies "what to sign" for a supplied key. It is deliberately not
	// derived from the recorded config (which would silently re-sign the whole
	// repository), only from the flag actually being set on this invocation.
	if f := cmd.Flags().Lookup("resign-packages"); f != nil && f.Changed {
		gf.signPackages = true
	}
	return validateSigningFlags()
}

// applyAWSEnv translates --profile/--region into the AWS_PROFILE/AWS_REGION
// environment variables consulted by the AWS SDK's default config loader. Only
// flags the user actually set are applied, so an existing environment (or the
// default profile) is left untouched otherwise.
//
// os.Setenv only fails on a malformed name, and both names here are constants.
func applyAWSEnv() {
	if gf.awsProfile != "" {
		_ = os.Setenv("AWS_PROFILE", gf.awsProfile)
	}
	if gf.awsRegion != "" {
		_ = os.Setenv("AWS_REGION", gf.awsRegion)
	}
}

// validateSigningFlags rejects ambiguous signing intent. In particular, a
// signing key with no instruction about what to sign is almost always a
// mistake (the key would otherwise be silently ignored), so we require the
// user to be explicit.
func validateSigningFlags() error {
	keyGiven := gf.gpgKey != "" || gf.gpgKeyID != ""
	if keyGiven && !gf.signMetadata && !gf.signPackages {
		return fmt.Errorf("a signing key was provided (--gpg-key/--gpg-key-id) but neither --sign-metadata nor --sign-packages was given; specify what to sign")
	}
	if gf.gpgKey != "" && gf.gpgKeyID != "" {
		return fmt.Errorf("specify only one of --gpg-key or --gpg-key-id")
	}
	return nil
}

// applyProfile resolves --target into defaults for compression and signature
// format. Flags the user set explicitly are left untouched.
func applyProfile(cmd *cobra.Command, _ []string) error {
	fl := cmd.Flags()
	comp, sig, err := resolveProfile(gf.target,
		gf.compression, gf.signatureFormat,
		fl.Changed("compression"), fl.Changed("signature-format"))
	if err != nil {
		return err
	}
	gf.compression, gf.signatureFormat = comp, sig
	return nil
}

// resolveProfile applies the named target profile's defaults to compression and
// signature format, unless the corresponding flag was set explicitly. It is a
// pure function so the profile logic can be tested in isolation.
func resolveProfile(target, compression, signature string, compressionSet, signatureSet bool) (comp, sig string, err error) {
	if target == "" {
		return compression, signature, nil
	}
	key := strings.ToLower(target)
	if canon, ok := profileAliases[key]; ok {
		key = canon
	}
	p, ok := profiles[key]
	if !ok {
		return compression, signature, fmt.Errorf("unknown --target %q (valid: rhel8, rhel9, rhel10 and aliases)", target)
	}
	if !compressionSet {
		compression = p.compression
	}
	if !signatureSet {
		signature = p.signature
	}
	return compression, signature, nil
}

// repoOptions builds repo.Options from the global flags, attaching a metadata
// signer when requested.
func repoOptions(create bool) (repo.Options, error) {
	opts := repo.Options{
		ChecksumType:           gf.checksum,
		Compression:            repo.Compression(gf.compression),
		LocationPrefix:         gf.locationPrefix,
		ChangelogLimit:         gf.changelogLimit,
		Create:                 create,
		DryRun:                 gf.dryRun,
		Force:                  gf.force,
		PruneBreakDeps:         gf.pruneBreakDeps,
		RemoveUnreferencedRPMs: gf.removeUnreferencedRPMs,
		RemoveStaleMetadata:    gf.removeStaleMetadata,
	}
	if gf.checksum != "sha256" {
		return opts, fmt.Errorf("only sha256 checksums are supported (got %q)", gf.checksum)
	}
	switch opts.Compression {
	case repo.GZIP, repo.ZSTD:
	default:
		return opts, fmt.Errorf("unsupported compression %q (use gzip or zstd)", gf.compression)
	}
	if gf.signMetadata {
		s, err := metadataSigner()
		if err != nil {
			return opts, err
		}
		opts.Signer = s
	}
	return opts, nil
}

func metadataSigner() (repo.Signer, error) {
	switch {
	case gf.gpgKey != "":
		return sign.NewKeyFileSigner(gf.gpgKey, gf.gpgPass)
	case gf.gpgKeyID != "":
		return sign.NewKeyIDSigner(gf.gpgKeyID), nil
	default:
		return nil, fmt.Errorf("--sign-metadata requires --gpg-key or --gpg-key-id")
	}
}

// ctx returns a context tied to the command.
func ctx(cmd *cobra.Command) context.Context {
	if c := cmd.Context(); c != nil {
		return c
	}
	return context.Background()
}
