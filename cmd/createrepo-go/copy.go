package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/repo"
	"github.com/danudey/createrepo-go/pkg/repoconfig"
	"github.com/danudey/createrepo-go/pkg/repodata"
	"github.com/danudey/createrepo-go/pkg/sign"
)

// copyFlags holds the copy-specific options.
type copyFlags struct {
	// destination state
	overwrite bool
	resume    bool
	update    bool

	// transforms
	rebuildMetadata bool
	resign          bool
	baseURL         string
	removeBaseURL   bool
	pruneOlder      bool
	repoName        string

	// selection
	latestOnly   bool
	include      []string
	exclude      []string
	arches       []string
	kinds        []string
	excludeKinds []string

	skipVerify bool
	assumeYes  bool
}

func copyCmd() *cobra.Command {
	var cf copyFlags
	cmd := &cobra.Command{
		Use:   "copy <source-repo> <destination-repo>",
		Short: "Copy a repository to another location",
		Long: `Read a repository from one location and write it to another. Source and
destination may each be any supported backend, so this mirrors between local
disk, SSH/SFTP, S3, GCS, and a read-only HTTP source.

By default the copy is exact: every file is transferred byte for byte, so the
destination is a replica of the source and its repomd.xml signature stays valid.
Every file whose checksum the metadata records is verified as it passes through,
and a file already present at the destination with matching content is not
transferred again — an interrupted copy resumes cheaply with --continue.

An exact copy is refused when it would produce a repository that misleads its
clients: if the source metadata pins packages to another host with
<location xml:base=...>, dnf would keep downloading them from there. Rewrite it
with --baseurl/--remove-baseurl, or pass --force to copy it verbatim anyway.

Any option that selects a subset of the packages, relocates them, rebuilds the
metadata, or re-signs anything switches the copy to rebuilding the destination's
metadata. That invalidates the source's repomd.xml signature, so when the source
is signed such an option requires re-signing the copy with --sign-metadata and a
key of your own.

Writing into a repository that already exists needs one of:
  --overwrite  replace it; files the source does not have are deleted
  --continue   resume an interrupted copy of this same source repository
  --update     merge the source's packages into it (an incremental copy)

Examples:
  # Mirror a public HTTP repository onto S3, exactly as it stands.
  createrepo-go copy https://downloads.example.com/el9 s3://my-bucket/el9

  # Resume after an interruption.
  createrepo-go copy https://downloads.example.com/el9 s3://my-bucket/el9 --continue

  # Take only the newest x86_64 build of each package, no debug or source rpms,
  # and sign the rebuilt metadata with our own key.
  createrepo-go copy /srv/upstream /srv/mirror \
      --latest-only --arch x86_64 --exclude-kinds debug,source \
      --sign-metadata --gpg-key-id releases@example.com

  # Pull this week's new packages into an existing mirror.
  createrepo-go copy /srv/upstream /srv/mirror --update`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCopy(cmd, args[0], args[1], &cf)
		},
	}

	f := cmd.Flags()
	f.BoolVar(&cf.overwrite, "overwrite", false, "replace an existing destination repository, deleting files the source does not have")
	f.BoolVar(&cf.resume, "continue", false, "resume an interrupted copy; fails if the destination is a different repository")
	f.BoolVar(&cf.update, "update", false, "add the source's packages to an existing destination repository")

	f.BoolVar(&cf.rebuildMetadata, "rebuild-metadata", false, "regenerate the metadata from the copied RPMs instead of carrying the source's over")
	f.BoolVar(&cf.resign, "resign-packages", false, "re-sign every copied RPM with the signing key, replacing the source's signature")
	f.StringVar(&cf.baseURL, "baseurl", "", "replace the metadata's <location xml:base> (and the recorded repo URL) with this URL")
	f.BoolVar(&cf.removeBaseURL, "remove-baseurl", false, "drop the metadata's <location xml:base> so clients fetch packages from the copy")
	f.BoolVar(&cf.pruneOlder, "prune-older", false, "after copying, drop superseded versions from the destination (--latest-only avoids transferring them in the first place)")
	f.BoolVar(&gf.pruneBreakDeps, "prune-break-deps", false, "when pruning, drop a version even if another package depends on it (default: keep it and warn)")
	f.StringVar(&cf.repoName, "repo-name", "", "human-readable repository name to record in the destination's config file")

	f.BoolVar(&cf.latestOnly, "latest-only", false, "copy only the newest version of each package name+arch")
	f.StringSliceVar(&cf.include, "include", nil, "only copy packages matching these glob patterns (name, name-version, or NEVRA; repeatable)")
	f.StringSliceVar(&cf.exclude, "exclude", nil, "do not copy packages matching these glob patterns (repeatable)")
	f.StringSliceVar(&cf.arches, "arch", nil, "only copy these architectures (noarch is always kept; source rpms are arch \"src\"); fails if an arch is absent from the source")
	f.StringSliceVar(&cf.kinds, "kinds", nil, "only copy these package kinds: binary, source, debuginfo, debugsource (alias: debug)")
	f.StringSliceVar(&cf.excludeKinds, "exclude-kinds", nil, "do not copy these package kinds")

	f.BoolVar(&cf.skipVerify, "skip-verify", false, "do not check GPG signatures even when a key is available")
	f.BoolVar(&gf.removeUnreferencedRPMs, "remove-unreferenced-rpms", false, "delete .rpm files at the destination the copied metadata does not reference")
	f.BoolVar(&gf.removeStaleMetadata, "remove-stale-metadata", false, "delete repodata files at the destination that are no longer in use")
	f.BoolVarP(&cf.assumeYes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

// runCopy drives the whole operation: open and verify the source, decide what
// the destination may look like, then either replicate it byte for byte or
// rebuild it from the selected packages.
func runCopy(cmd *cobra.Command, srcLoc, dstLoc string, cf *copyFlags) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	if err := cf.validate(); err != nil {
		return err
	}

	// --- source ---------------------------------------------------------
	srcBE, err := backend.Open(ctx(cmd), srcLoc)
	if err != nil {
		return err
	}
	srcCfg, err := repoconfig.Load(ctx(cmd), srcBE)
	if err != nil {
		closeBackend(srcBE)
		return err
	}
	src, err := repo.OpenWith(ctx(cmd), srcBE, repo.Options{})
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	defer src.Close()

	keyrings, releaseKeys := verificationKeyrings(cmd, srcCfg, cf.skipVerify)
	defer releaseKeys()

	srcSig, err := src.MetadataSignature(ctx(cmd))
	if err != nil {
		return fmt.Errorf("source repomd.xml.asc: %w", err)
	}
	verifier, closeVerifier, err := verifySourceMetadata(cmd, src, srcSig, keyrings, cf.skipVerify)
	if err != nil {
		return err
	}
	defer closeVerifier()

	// --- selection ------------------------------------------------------
	filter, err := cf.filter()
	if err != nil {
		return err
	}
	all := src.Index().Packages()
	selected, warnings, err := filter.Apply(all)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(errOut, "warning:", w)
	}
	// An empty source repository is a legitimate thing to copy; an empty
	// selection out of a non-empty one means the filters matched nothing.
	if len(selected) == 0 && len(all) > 0 {
		return fmt.Errorf("no packages selected from %s (%d in the source); check --include/--exclude/--arch/--kinds", srcLoc, len(all))
	}

	// --- destination state ----------------------------------------------
	dstBE, err := backend.Open(ctx(cmd), dstLoc)
	if err != nil {
		return err
	}
	dstExists, err := repoExists(cmd, dstBE)
	if err != nil {
		closeBackend(dstBE)
		return err
	}
	if dstExists && !cf.overwrite && !cf.resume && !cf.update {
		closeBackend(dstBE)
		return fmt.Errorf("a repository already exists at %s; pass --overwrite to replace it, --continue to resume an interrupted copy, or --update to add to it", dstLoc)
	}
	if !dstExists && cf.update {
		closeBackend(dstBE)
		return fmt.Errorf("--update needs an existing repository at %s; there is none", dstLoc)
	}

	// --- mode -----------------------------------------------------------
	baseURLs := repo.XMLBases(all)
	changeBaseURL := cf.removeBaseURL || cmd.Flags().Changed("baseurl")
	relocate := cmd.Flags().Changed("location-prefix")
	transform := cf.update || cf.rebuildMetadata || cf.resign || cf.pruneOlder ||
		!filter.Empty() || relocate || (changeBaseURL && len(baseURLs) > 0)

	if !transform && len(baseURLs) > 0 && !gf.force {
		closeBackend(dstBE)
		return fmt.Errorf("the source metadata sends clients to %s for its packages (<location xml:base>), so an exact copy at %s would still serve them from there; "+
			"rewrite it with --remove-baseurl or --baseurl URL, or pass --force to copy the metadata verbatim",
			strings.Join(baseURLs, ", "), dstLoc)
	}
	if srcCfg != nil && srcCfg.BaseURL != "" && !changeBaseURL {
		fmt.Fprintf(errOut, "warning: the source records baseurl %s in %s; the copy inherits it (set --baseurl/--remove-baseurl to change it)\n",
			srcCfg.BaseURL, repoconfig.Path)
	}

	if cf.resume {
		if err := checkResumable(cmd, srcLoc, dstBE, dstExists, src); err != nil {
			closeBackend(dstBE)
			return err
		}
	}

	if !transform {
		return copyExact(cmd, src, srcLoc, srcCfg, srcSig, dstBE, dstLoc, dstExists, cf, verifier)
	}

	sourceSigned := srcSig != nil || (srcCfg != nil && srcCfg.SignMetadata)
	return copyRebuilding(cmd, copyRun{
		src: src, srcLoc: srcLoc, srcCfg: srcCfg, sourceSigned: sourceSigned,
		dstBE: dstBE, dstLoc: dstLoc, dstExists: dstExists,
		selected: selected, cf: cf, verifier: verifier,
		changeBaseURL: changeBaseURL, relocate: relocate,
		out: out, errOut: errOut,
	})
}

// validate rejects incoherent flag combinations before anything is opened.
func (cf *copyFlags) validate() error {
	set := 0
	for _, b := range []bool{cf.overwrite, cf.resume, cf.update} {
		if b {
			set++
		}
	}
	if set > 1 {
		return fmt.Errorf("--overwrite, --continue and --update are mutually exclusive")
	}
	if cf.removeBaseURL && cf.baseURL != "" {
		return fmt.Errorf("specify only one of --baseurl or --remove-baseurl")
	}
	return nil
}

// filter builds the package selection from the copy flags.
func (cf *copyFlags) filter() (repodata.Filter, error) {
	kinds, err := repodata.ParseKinds(cf.kinds)
	if err != nil {
		return repodata.Filter{}, err
	}
	excludeKinds, err := repodata.ParseKinds(cf.excludeKinds)
	if err != nil {
		return repodata.Filter{}, err
	}
	return repodata.Filter{
		Include:      cf.include,
		Exclude:      cf.exclude,
		Arches:       cf.arches,
		Kinds:        kinds,
		ExcludeKinds: excludeKinds,
		LatestOnly:   cf.latestOnly,
	}, nil
}

// repoExists reports whether a repository has already been published at be.
func repoExists(cmd *cobra.Command, be backend.Backend) (bool, error) {
	_, err := be.Stat(ctx(cmd), "repodata/repomd.xml")
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, backend.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// verificationKeyrings resolves the public keys the copy validates against:
// the ones the user supplied with --keyring, or failing that the source
// repository's own signing key, exported from the local GnuPG keyring by the
// fingerprint recorded in its config. A key that cannot be resolved is a
// warning, not an error — verification is then simply not possible.
func verificationKeyrings(cmd *cobra.Command, srcCfg *repoconfig.Config, skip bool) ([]string, func()) {
	noop := func() {}
	if skip {
		return nil, noop
	}
	if len(gf.keyrings) > 0 {
		return gf.keyrings, noop
	}
	if srcCfg == nil || srcCfg.GPGKeyID == "" {
		return nil, noop
	}
	path, cleanup, err := sign.ExportPublicKey(srcCfg.GPGKeyID)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: the source repository is signed with %s but that key is not available locally (%v); signatures will not be verified. Pass --keyring to supply it.\n",
			srcCfg.GPGKeyID, err)
		return nil, noop
	}
	fmt.Fprintf(cmd.OutOrStdout(), "verifying against the source's recorded signing key %s\n", srcCfg.GPGKeyID)
	return []string{path}, cleanup
}

// verifySourceMetadata checks the source's detached repomd.xml signature and
// prepares the package-signature verifier used for each RPM the copy reads. It
// returns a nil verifier when there is no key to check against.
func verifySourceMetadata(cmd *cobra.Command, src *repo.Repo, srcSig []byte, keyrings []string, skip bool) (*sign.Verifier, func(), error) {
	noop := func() {}
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	if skip {
		if srcSig != nil {
			fmt.Fprintln(errOut, "warning: --skip-verify: the source's signatures are not being checked")
		}
		return nil, noop, nil
	}
	if len(keyrings) == 0 {
		if gf.verifySigs {
			return nil, noop, fmt.Errorf("--verify-sigs needs a public key: pass --keyring, or copy a repository whose %s records its signing key", repoconfig.Path)
		}
		if srcSig != nil {
			fmt.Fprintf(errOut, "warning: the source's repomd.xml is signed but no public key is available to check it; pass --keyring to verify the copy\n")
		}
		return nil, noop, nil
	}

	if srcSig != nil {
		fpr, err := sign.VerifyDetached(src.RawRepomd(), srcSig, keyrings)
		if err != nil {
			return nil, noop, fmt.Errorf("the source's repomd.xml signature does not verify: %w", err)
		}
		fmt.Fprintf(out, "verified: repomd.xml signed by %s\n", fpr)
	} else {
		fmt.Fprintln(errOut, "warning: the source's metadata is not signed (no repodata/repomd.xml.asc)")
	}

	v, err := sign.NewVerifier(keyrings...)
	if err != nil {
		// Package verification needs the rpmkeys tool. Without --verify-sigs
		// the operator did not ask for it, so its absence must not stop a copy
		// that is otherwise fine — the metadata signature above still held.
		if gf.verifySigs {
			return nil, noop, err
		}
		fmt.Fprintf(errOut, "warning: package signatures cannot be verified here (%v); metadata and checksums are still checked\n", err)
		return nil, noop, nil
	}
	return v, v.Close, nil
}

// checkResumable confirms that --continue is resuming a copy of this same
// source: the destination must record the same origin, or hold nothing the
// source does not have.
func checkResumable(cmd *cobra.Command, srcLoc string, dstBE backend.Backend, dstExists bool, src *repo.Repo) error {
	dstCfg, err := repoconfig.Load(ctx(cmd), dstBE)
	if err != nil {
		return err
	}
	if dstCfg != nil && dstCfg.CopySource != "" && dstCfg.CopySource != srcLoc {
		return fmt.Errorf("--continue: %s was copied from %s, not from %s", dstBE, dstCfg.CopySource, srcLoc)
	}
	if !dstExists {
		return nil
	}
	dst, err := repo.OpenWith(ctx(cmd), dstBE, repo.Options{})
	if err != nil {
		return fmt.Errorf("--continue: reading the destination: %w", err)
	}
	// The backend is shared with the caller, which owns closing it.
	if err := repo.SameRepository(src.Index(), dst.Index()); err != nil {
		return fmt.Errorf("--continue: %s is not a partial copy of %s: %w", dstBE, srcLoc, err)
	}
	return nil
}

// normalizeKeyID reduces a fingerprint or key id to the lowercase long key id
// (its last 16 hex digits), so a fingerprint recorded in a config file can be
// compared with the issuer of an actual package signature.
func normalizeKeyID(s string) string {
	s = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	if len(s) > 16 {
		return s[len(s)-16:]
	}
	return s
}
