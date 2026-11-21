package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/spf13/cobra"

	"github.com/danudey/createrepo-go/internal/cli"
	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/checkcore"
	"github.com/danudey/createrepo-go/pkg/gpg"
	"github.com/danudey/createrepo-go/pkg/progress"
	"github.com/danudey/createrepo-go/pkg/rpm/pkgcheck"
	"github.com/danudey/createrepo-go/pkg/rpm/repo"
	"github.com/danudey/createrepo-go/pkg/rpm/repoconfig"
	"github.com/danudey/createrepo-go/pkg/rpm/repofile"
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

	// keyringURLs are public keys to trust, fetched like a .repo gpgkey=.
	keyringURLs []string
	// releasever, arch and vars give values to a .repo file's variables.
	releasever string
	arch       string
	vars       []string
}

func verifyCmd() *cobra.Command {
	var vf verifyFlags
	cmd := &cobra.Command{
		Use:   "verify <repo-url | file.repo>",
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
--keyring files and published keys together, or failing both from the recorded
key in the local GnuPG keyring; without any, signatures are checked for
integrity but not for who made them.

Published keys are the public keys your users install: --keyring-url (http(s)
or file://, repeatable), else the gpg_key_url createrepo-go.json records. When
there is one, every signature present, repomd.xml's included, must verify
against the trusted keys; one that does not is a signature your users cannot
verify either.

Instead of a repository, verify accepts a .repo file (a path, file:// or
http(s) URL ending in .repo) and verifies every enabled repository in it the
way a client using that file would: its first baseurl, its gpgkey= keys as the
published keys, and its switches as requirements. gpgcheck=1 requires every
RPM to carry a signature the keys verify; repo_gpgcheck=1 does the same for
repomd.xml. Variables in the file are substituted from --releasever, --arch
($arch and $basearch) and --var name=value; a variable with no value is an
error.

  --require-trusted          every signature must verify against a trusted key
  --require-signed-packages  every RPM must be signed (implied when the config
                             records sign_packages)
  --require-signed-metadata  repomd.xml must be signed (implied when the config
                             records sign_metadata)

  createrepo-go verify https://downloads.example.com/el9 \
      --keyring-url https://downloads.example.com/RPM-GPG-KEY-example
  createrepo-go verify ./example.repo --releasever 9 --arch x86_64`,
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
	f.StringArrayVar(&vf.keyringURLs, "keyring-url", nil, "URL of a public key to trust, as your users fetch it (http(s) or file://; repeatable). Every signature present must verify against the trusted keys")
	f.StringVar(&vf.releasever, "releasever", "", "value for $releasever (and $releasever_major/_minor) in a .repo file")
	f.StringVar(&vf.arch, "arch", "", "value for $arch and $basearch in a .repo file")
	f.StringArrayVar(&vf.vars, "var", nil, "value for any other .repo file variable, as name=value (repeatable)")
	return cmd
}

func runVerify(cmd *cobra.Command, input string, vf verifyFlags) error {
	if isRepoFile(input) {
		return runVerifyRepoFile(cmd, input, vf)
	}
	t := verifyTarget{}
	var err error
	if t.keys, t.keySources, err = loadKeyURLs(cli.Ctx(cmd), vf.keyringURLs); err != nil {
		return err
	}
	return verifyOne(cmd, input, vf, t)
}

// verifyTarget is what verifying one repository takes beyond its location:
// the published keys its users verify it with, and, for a repository named by
// a .repo file, what that file tells clients to check.
type verifyTarget struct {
	// label prefixes every line printed about the repository.
	label string
	// keys are published public keys (--keyring-url, a .repo gpgkey=); any
	// signature present must verify against them.
	keys       openpgp.EntityList
	keySources []string
	// fromRepoFile says gpgCheck and repoGPGCheck are a .repo file's
	// settings, which decide what has to be signed and trusted.
	fromRepoFile bool
	gpgCheck     bool
	repoGPGCheck bool
	// problems found before the repository was opened (a gpgkey that could
	// not be loaded), reported with its results.
	problems []string
}

// verifyOne verifies the repository at location, returning an error when it
// fails.
func verifyOne(cmd *cobra.Command, location string, vf verifyFlags, t verifyTarget) error {
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

	vopt, err := verifyOptions(cmd, cfg, vf, &t)
	if err != nil {
		return err
	}
	if len(t.keySources) > 0 {
		fmt.Fprintf(out, "%strusted keys: %s\n", t.label, strings.Join(t.keySources, ", "))
	}
	if vf.checksums {
		vopt.Checksums = repo.ChecksumContent
		// A full content check can be a large transfer; say so before
		// starting rather than appearing to hang.
		if n, size := r.DownloadEstimate(); n > 0 {
			fmt.Fprintf(out, "%shashing %d package(s) from %s: ~%s to download\n", t.label, n, r.Backend(), progress.Bytes(size))
		}
	}

	res, err := r.Verify(ctx, vopt)
	if err != nil {
		return err
	}
	for _, p := range res.Warnings {
		fmt.Fprintln(errOut, t.label+"WARNING:", p)
	}
	for _, p := range t.problems {
		fmt.Fprintln(errOut, t.label+"PROBLEM:", p)
	}
	for _, p := range res.Problems {
		fmt.Fprintln(errOut, t.label+"PROBLEM:", p)
	}
	if n := len(res.Problems) + len(t.problems); n > 0 {
		return fmt.Errorf("%d problem(s) found", n)
	}
	fmt.Fprintln(out, t.label+verifySummary(res, !vf.skipPackageContents))
	return nil
}

// verifyOptions resolves what verify expects of the repository: the target
// whose rpm judges the signatures, the trusted keys, the key the repository
// records, and which signatures are required.
func verifyOptions(cmd *cobra.Command, cfg *repoconfig.Config, vf verifyFlags, t *verifyTarget) (repo.VerifyOptions, error) {
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
		// The key the repository says its users install is checked for
		// consistency with the signatures, unless the operator named keys.
		// The repository chose it, so it is never trusted: a repository
		// could otherwise vouch for itself with a key of its own choosing.
		if len(t.keys) == 0 && cfg.GPGKeyURL != "" {
			checkRecordedKeyURL(cmd, cfg.GPGKeyURL, &opt, t)
		}
	}

	switch {
	case t.fromRepoFile:
		// What the .repo file has clients check is what has to hold.
		opt.RequireSignedPackages = opt.RequireSignedPackages || t.gpgCheck
		opt.RequireTrusted = opt.RequireTrusted || t.gpgCheck
		opt.RequireSignedMetadata = opt.RequireSignedMetadata || t.repoGPGCheck
		opt.RequireTrustedMetadata = t.repoGPGCheck
	case len(t.keys) > 0:
		// A published key is the one users verify with: a signature it does
		// not verify is one they cannot.
		opt.RequireTrusted = true
	}

	kr, authoritative, err := trustedKeys(errOut, recordedKey, t.keys)
	if err != nil {
		return opt, err
	}
	opt.Keyring = kr
	if recordedKey != "" {
		opt.ExpectedKeyIDs, opt.ExpectedKeyIncomplete = expectedKeyIDs(recordedKey, authoritative, t.keys)
	}
	if (opt.RequireTrusted || opt.RequireTrustedMetadata) && len(kr) == 0 {
		if t.fromRepoFile {
			return opt, fmt.Errorf("the .repo file turns on gpgcheck or repo_gpgcheck but no key could be loaded to check them: give it a gpgkey=, or pass --keyring or --keyring-url")
		}
		return opt, fmt.Errorf("--require-trusted needs trusted keys: pass --keyring or --keyring-url, or verify a repository whose %s records a signing key that is in the local GnuPG keyring", repoconfig.Path)
	}
	return opt, nil
}

// expectedKeyIDs expands the recorded signing key into the long key ids that
// may sign on its behalf — its own and its subkeys' — from the --keyring
// files, the local GnuPG keyring and the published keys together. A published
// copy is what is under test and can lack a subkey, so finding the key only
// there leaves the list incomplete: a signature by another key id may still
// be a subkey's, and is then a warning rather than a problem.
func expectedKeyIDs(recorded string, authoritative, published openpgp.EntityList) (ids []string, incomplete bool) {
	want := strings.ToLower(strings.ReplaceAll(recorded, " ", ""))
	seen := map[string]bool{}
	addID := func(id string) {
		id = strings.ToLower(id)
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	found := false
	expand := func(kr openpgp.EntityList, complete bool) {
		for _, e := range kr {
			if !strings.HasSuffix(strings.ToLower(gpg.FingerprintOf(e)), want) {
				continue
			}
			found = found || complete
			addID(e.PrimaryKey.KeyIdString())
			for _, sk := range e.Subkeys {
				addID(sk.PublicKey.KeyIdString())
			}
		}
	}
	expand(authoritative, true)
	expand(published, false)
	_, cleanup, err := sign.ExportPublicKey(recorded)
	cleanup()
	for _, id := range sign.KeyAndSubkeyIDs(recorded) {
		addID(id)
	}
	return ids, !found && err != nil
}

// trustedKeys loads the keys signatures are verified against: --keyring and
// the published keys together, or failing both the repository's recorded key
// from the local GnuPG keyring. No keys is not an error; signatures are then
// checked for integrity only. authoritative is the part not under test: the
// --keyring files, or the local keyring's copy.
func trustedKeys(errOut io.Writer, recordedKey string, published openpgp.EntityList) (kr, authoritative openpgp.EntityList, err error) {
	if len(gf.keyrings) > 0 {
		if authoritative, err = gpg.LoadKeyring(gf.keyrings...); err != nil {
			return nil, nil, err
		}
	}
	kr = append(slices.Clone(authoritative), published...)
	if len(kr) > 0 || recordedKey == "" {
		return kr, authoritative, nil
	}
	path, cleanup, err := sign.ExportPublicKey(recordedKey)
	if err != nil {
		fmt.Fprintf(errOut, "warning: the repository's signing key %s is not in the local GnuPG keyring (%v); signatures are checked for integrity and signer, but not verified. Pass --keyring or --keyring-url to supply it.\n", recordedKey, err)
		return nil, nil, nil
	}
	defer cleanup()
	kr, err = gpg.LoadKeyring(path)
	return kr, kr, err
}

// checkRecordedKeyURL fetches the public key a repository records as the one
// its users install, for opt.PublishedKeys. Only http(s) is fetched (the
// config's own validation already refuses anything else), and plain http is
// called out, since a key fetched that way can be swapped in transit.
func checkRecordedKeyURL(cmd *cobra.Command, keyURL string, opt *repo.VerifyOptions, t *verifyTarget) {
	keys, err := gpg.FetchPublicKey(cli.Ctx(cmd), nil, keyURL)
	if err != nil {
		t.problems = append(t.problems, fmt.Sprintf("the public key %s records at %s cannot be fetched, so users cannot install it either: %v", repoconfig.Path, keyURL, err))
		return
	}
	if u, err := url.Parse(keyURL); err == nil && u.Scheme == "http" {
		fmt.Fprintf(cmd.ErrOrStderr(), "%swarning: the recorded public key URL %s uses plain http; anyone who can tamper with the connection can hand users a different key\n", t.label, keyURL)
	}
	opt.PublishedKeys, opt.PublishedKeySource = keys, keyURL
	fmt.Fprintf(cmd.OutOrStdout(), "%spublished key (recorded gpg_key_url, checked for consistency, not trusted): %s\n", t.label, keyURL)
}

// loadKeyURLs loads the public keys at each URL; any that cannot be loaded is
// an error, because the caller named it.
func loadKeyURLs(ctx context.Context, urls []string) (openpgp.EntityList, []string, error) {
	var all openpgp.EntityList
	for _, u := range urls {
		keys, err := gpg.LoadPublicKeys(ctx, nil, u)
		if err != nil {
			return nil, nil, fmt.Errorf("--keyring-url %s: %w", u, err)
		}
		all = append(all, keys...)
	}
	return all, urls, nil
}

// isRepoFile reports whether verify's argument names a .repo file rather
// than a repository.
func isRepoFile(input string) bool {
	p := input
	if strings.Contains(input, "://") {
		if u, err := url.Parse(input); err == nil {
			p = u.Path
		}
	}
	return strings.HasSuffix(strings.ToLower(p), ".repo")
}

// runVerifyRepoFile verifies every enabled repository a .repo file defines,
// as a client configured by that file would check it.
func runVerifyRepoFile(cmd *cobra.Command, input string, vf verifyFlags) error {
	ctx := cli.Ctx(cmd)
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	data, err := readRepoFile(ctx, input)
	if err != nil {
		return err
	}
	sections, err := repofile.Parse(data)
	if err != nil {
		return fmt.Errorf("%s: %w", input, err)
	}
	vars, err := repoVars(vf)
	if err != nil {
		return err
	}
	extraKeys, extraSources, err := loadKeyURLs(ctx, vf.keyringURLs)
	if err != nil {
		return err
	}

	// Every variable is resolved before anything is fetched, so a missing
	// value is reported once, up front, rather than after other repositories
	// were verified.
	var enabled []repofile.Section
	var varErrs []string
	for _, s := range sections {
		if !s.Enabled {
			fmt.Fprintf(out, "[%s] skipped: enabled=0\n", s.ID)
			continue
		}
		exp, err := s.Expand(vars)
		if err != nil {
			varErrs = append(varErrs, err.Error())
			continue
		}
		enabled = append(enabled, exp)
	}
	if len(varErrs) > 0 {
		return fmt.Errorf("%s: %s; supply values with --releasever, --arch or --var name=value", input, strings.Join(varErrs, "; "))
	}
	if len(enabled) == 0 {
		return fmt.Errorf("%s defines no enabled repository", input)
	}

	failed := 0
	for _, s := range enabled {
		label := "[" + s.ID + "] "
		if err := verifySection(cmd, input, s, label, vf, extraKeys, extraSources); err != nil {
			fmt.Fprintf(errOut, "%s%v\n", label, err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d repositor(ies) in %s failed verification", failed, len(enabled), input)
	}
	return nil
}

// verifySection verifies one repository from a .repo file.
func verifySection(cmd *cobra.Command, input string, s repofile.Section, label string, vf verifyFlags, extraKeys openpgp.EntityList, extraSources []string) error {
	out := cmd.OutOrStdout()
	if len(s.BaseURLs) == 0 {
		if s.MirrorList != "" {
			return fmt.Errorf("it has only a mirrorlist/metalink, which verify cannot follow; verify a mirror's URL directly")
		}
		return fmt.Errorf("it has no baseurl")
	}
	base := s.BaseURLs[0]
	if err := checkcore.CheckTargetScheme(input, base); err != nil {
		return err
	}
	fmt.Fprintf(out, "%sverifying %s (gpgcheck=%s, repo_gpgcheck=%s)\n", label, base, onOff(s.GPGCheck), onOff(s.RepoGPGCheck))
	if len(s.BaseURLs) > 1 {
		fmt.Fprintf(out, "%sonly the first of %d baseurl entries is verified\n", label, len(s.BaseURLs))
	}

	t := verifyTarget{
		label:        label,
		fromRepoFile: true,
		gpgCheck:     s.GPGCheck,
		repoGPGCheck: s.RepoGPGCheck,
		keys:         slices.Clone(extraKeys),
		keySources:   slices.Clone(extraSources),
	}
	for _, k := range s.GPGKeys {
		keys, err := gpg.LoadPublicKeys(cli.Ctx(cmd), nil, k)
		if err != nil {
			t.problems = append(t.problems, fmt.Sprintf("gpgkey %s cannot be loaded, so clients cannot import it either: %v", k, err))
			continue
		}
		t.keys = append(t.keys, keys...)
		t.keySources = append(t.keySources, k)
	}
	if (s.GPGCheck || s.RepoGPGCheck) && len(s.GPGKeys) == 0 {
		t.problems = append(t.problems, "gpgcheck or repo_gpgcheck is on but there is no gpgkey=, so a client has no key to import and refuses the repository unless the key is already installed")
	}
	return verifyOne(cmd, base, vf, t)
}

func onOff(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// readRepoFile reads a .repo file from local disk (a path or file:// URL) or
// over http(s).
func readRepoFile(ctx context.Context, input string) ([]byte, error) {
	u, err := url.Parse(input)
	if err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		data, err := checkcore.FetchOverHTTP(ctx, 30*time.Second, "createrepo-go/"+version)(input)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", input, err)
		}
		return data, nil
	}
	path := input
	if err == nil && u.Scheme == "file" {
		path = u.Path
	}
	return os.ReadFile(path)
}

// repoVars collects the values for a .repo file's variables. --arch fills
// both $arch and $basearch, as dnf derives one from the other on most hosts.
func repoVars(vf verifyFlags) (map[string]string, error) {
	vars := map[string]string{}
	if vf.releasever != "" {
		vars["releasever"] = vf.releasever
		// dnf derives these from $releasever.
		major, minor, _ := strings.Cut(vf.releasever, ".")
		vars["releasever_major"] = major
		vars["releasever_minor"] = minor
	}
	if vf.arch != "" {
		vars["arch"] = vf.arch
		vars["basearch"] = vf.arch
	}
	for _, kv := range vf.vars {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("--var %q: want name=value", kv)
		}
		vars[strings.TrimPrefix(name, "$")] = value
	}
	return vars, nil
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
