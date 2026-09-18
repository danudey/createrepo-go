package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/repo"
	"github.com/danudey/createrepo-go/pkg/repoconfig"
	"github.com/danudey/createrepo-go/pkg/repodata"
	"github.com/danudey/createrepo-go/pkg/sign"
)

// copyExact replicates the source repository file by file. Nothing is
// regenerated, so the destination is byte-identical to the source and any
// detached repomd.xml signature it carries stays valid.
func copyExact(cmd *cobra.Command, src *repo.Repo, srcLoc string, srcCfg *repoconfig.Config, srcSig []byte,
	dstBE backend.Backend, dstLoc string, dstExists bool, cf *copyFlags, verifier *sign.Verifier,
) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	defer closeBackend(dstBE)

	objs, listed, err := src.Objects(ctx(cmd))
	if err != nil {
		return err
	}
	if !listed {
		fmt.Fprintf(errOut, "warning: %s cannot list its contents, so only the files the metadata references are copied\n", src.Backend())
	}

	prune := cf.overwrite && dstExists
	fmt.Fprintf(out, "Copy %s -> %s (exact, %d object(s), %s)\n",
		srcLoc, dstLoc, len(objs), humanBytes(totalObjectSize(objs)))
	if prune {
		fmt.Fprintf(out, "  --overwrite: files at the destination that the source does not have will be deleted\n")
		if !cf.assumeYes && !gf.dryRun {
			if !promptYesNo(cmd, "Proceed?") {
				fmt.Fprintln(out, "aborted; no changes made")
				return nil
			}
		}
	}

	prefix := ""
	if gf.dryRun {
		prefix = dryRunPrefix
	}
	stats, err := repo.CopyExact(ctx(cmd), src.Backend(), dstBE, objs, repo.CopyOptions{
		DryRun: gf.dryRun,
		Force:  gf.force,
		Prune:  prune,
		Inspect: func(obj repo.SourceObject, local string) error {
			if verifier == nil || obj.Kind != repo.ObjectPackage {
				return nil
			}
			return verifier.VerifyFile(local)
		},
		Progress: func(action string, obj repo.SourceObject) {
			fmt.Fprintf(out, "%s%-7s %s\n", prefix, action, obj.Path)
		},
	})
	if err != nil {
		return err
	}

	if err := resignCopiedMetadata(cmd, src, dstBE, srcSig); err != nil {
		return err
	}

	// The source's config file came across verbatim; patch in this copy's
	// origin and any base URL change.
	cfg := repoconfig.Config{}
	if srcCfg != nil {
		cfg = *srcCfg
	}
	applyCopyConfig(&cfg, srcLoc, cf)
	writeRepoConfig(cmd, dstBE, &cfg)

	fmt.Fprintf(out, "%s%d object(s) copied (%s), %d already present%s\n",
		prefix, stats.Copied, humanBytes(stats.Bytes), stats.Skipped, deletedSuffix(stats))
	return nil
}

// resignCopiedMetadata replaces the copied repomd.xml.asc with a signature made
// by this operator's key, when --sign-metadata was given. The copied repomd.xml
// is byte-identical to the source's, so the new signature is over the same
// document the source signed.
func resignCopiedMetadata(cmd *cobra.Command, src *repo.Repo, dstBE backend.Backend, srcSig []byte) error {
	if !gf.signMetadata {
		return nil
	}
	signer, err := metadataSigner()
	if err != nil {
		return err
	}
	sig, err := signer.SignDetached(src.RawRepomd())
	if err != nil {
		return fmt.Errorf("sign repomd.xml: %w", err)
	}
	out := cmd.OutOrStdout()
	if gf.dryRun {
		fmt.Fprintln(out, "[dry-run] sign    repodata/repomd.xml.asc")
		return nil
	}
	if err := dstBE.Put(ctx(cmd), "repodata/repomd.xml.asc", bytes.NewReader(sig), int64(len(sig))); err != nil {
		return fmt.Errorf("write repomd.xml.asc: %w", err)
	}
	verb := "signed"
	if srcSig != nil {
		verb = "re-signed"
	}
	fmt.Fprintf(out, "%s repodata/repomd.xml.asc\n", verb)
	return nil
}

// copyRun carries the state a rebuilding copy needs.
type copyRun struct {
	src          *repo.Repo
	srcLoc       string
	srcCfg       *repoconfig.Config
	sourceSigned bool

	dstBE     backend.Backend
	dstLoc    string
	dstExists bool

	selected []*repodata.Package
	cf       *copyFlags
	verifier *sign.Verifier

	changeBaseURL bool
	relocate      bool

	out    io.Writer
	errOut io.Writer
}

// copyRebuilding copies the selected packages and regenerates the destination's
// metadata around them. It is the path taken whenever an option makes the copy
// something other than a verbatim replica.
func copyRebuilding(cmd *cobra.Command, run copyRun) error {
	cf := run.cf
	dst, prev, err := openRepoBackend(cmd, run.dstBE, true)
	if err != nil {
		return err
	}
	defer dst.Close()

	// Rebuilt metadata is a different document from the one the source signed,
	// so a signed source may only be transformed if the copy gets a signature
	// of its own.
	keyGiven := gf.gpgKey != "" || gf.gpgKeyID != ""
	signingCopy := gf.signMetadata && keyGiven
	if run.sourceSigned && !signingCopy {
		return fmt.Errorf("this copy rebuilds the metadata (%s), which invalidates the signature the source carries; "+
			"re-sign the copy with --sign-metadata and --gpg-key/--gpg-key-id, or copy the repository unchanged",
			strings.Join(transformReasons(cmd, run), ", "))
	}

	if cf.update && !cf.resign {
		if err := checkPackageSigners(cmd, run, dst, prev); err != nil {
			return err
		}
	}

	if cf.overwrite {
		existing := dst.Index().Len()
		dst.ClearIndex()
		if run.dstExists && !cf.assumeYes && !gf.dryRun {
			fmt.Fprintf(run.out, "--overwrite: the %d package(s) already at %s are dropped, and any RPM the new metadata does not reference is deleted\n",
				existing, run.dstLoc)
			if !promptYesNo(cmd, "Proceed?") {
				fmt.Fprintln(run.out, "aborted; no changes made")
				return nil
			}
		}
	}

	signFn, releaseSigner, err := copyPackageSigner(cmd, cf, dst)
	if err != nil {
		return err
	}
	defer releaseSigner()

	prefix := ""
	if gf.dryRun {
		prefix = dryRunPrefix
	}
	fmt.Fprintf(run.out, "Copy %s -> %s (rebuilding metadata, %d package(s))\n", run.srcLoc, run.dstLoc, len(run.selected))

	stats, err := dst.CopyPackagesFrom(ctx(cmd), run.src.Backend(), run.selected, repo.PackageCopyOptions{
		LocationPrefix:  gf.locationPrefix,
		Relocate:        run.relocate,
		SetXMLBase:      run.changeBaseURL,
		XMLBase:         cf.baseURL,
		RebuildMetadata: cf.rebuildMetadata,
		Sign:            signFn,
		Inspect: func(p *repodata.Package, local string) error {
			if run.verifier == nil {
				return nil
			}
			if err := run.verifier.VerifyFile(local); err != nil {
				return fmt.Errorf("%s: %w", p.NEVRA(), err)
			}
			return nil
		},
		Progress: func(action string, p *repodata.Package, loc string) {
			fmt.Fprintf(run.out, "%s%-7s %s -> %s\n", prefix, action, p.NEVRA(), loc)
		},
	})
	if err != nil {
		return err
	}

	// Pruning reconciles the destination once every copied package is in the
	// index, so it also drops versions the destination already held.
	if cf.pruneOlder {
		rep := dst.PruneOlderVersions()
		printPruneWarnings(cmd, rep.Kept, rep.Broken)
		for _, p := range rep.Removed {
			fmt.Fprintf(run.out, "%spruned  %s\n", prefix, p.NEVRA())
		}
	}

	plan, err := dst.Commit(ctx(cmd))
	if err != nil {
		return err
	}
	printPlan(cmd, plan, gf.dryRun)
	for _, dp := range dst.CheckDependencies() {
		fmt.Fprintf(run.errOut, "warning: unmet dependency after copy: %s\n", dp)
	}

	cfg := effectiveConfig(cmd, prev, "", "")
	if run.srcCfg != nil {
		if cfg.Name == "" {
			cfg.Name = run.srcCfg.Name
		}
		if cfg.BaseURL == "" {
			cfg.BaseURL = run.srcCfg.BaseURL
		}
	}
	applyCopyConfig(&cfg, run.srcLoc, cf)
	writeRepoConfig(cmd, dst.Backend(), &cfg)

	fmt.Fprintf(run.out, "%s%d package(s) transferred (%s), %d already present\n",
		prefix, stats.Copied, humanBytes(stats.Bytes), stats.Skipped)
	return nil
}

// copyPackageSigner builds the per-package signer for a copy, if one was asked
// for. --resign-packages replaces the source's signature; --sign-packages adds
// one (for an unsigned source). Either way the RPM's bytes change, so the
// destination copy is always overwritten.
func copyPackageSigner(cmd *cobra.Command, cf *copyFlags, dst *repo.Repo) (func(string) (string, func(), error), func(), error) {
	noop := func() {}
	if !cf.resign && !cmd.Flags().Changed("sign-packages") {
		return nil, noop, nil
	}
	signer, cleanup, err := packageSigner()
	if err != nil {
		return nil, noop, err
	}
	signer.Replace = cf.resign
	dst.SetForce(true)
	return signer.SignFile, cleanup, nil
}

// checkPackageSigners refuses an --update that would mix packages signed by
// different keys, which would leave the merged repository unusable for any
// client that trusts only one of them. The recorded key fingerprints settle it
// when both repositories have one; otherwise one package from each side is
// downloaded and its signature read.
func checkPackageSigners(cmd *cobra.Command, run copyRun, dst *repo.Repo, dstCfg *repoconfig.Config) error {
	srcSig, err := signerIDs(cmd, configKeyID(run.srcCfg), run.src, run.selected)
	if err != nil {
		return err
	}
	dstSig, err := signerIDs(cmd, configKeyID(dstCfg), dst, dst.Index().Packages())
	if err != nil {
		return err
	}

	switch {
	case !srcSig.signed && !dstSig.signed:
		return nil // neither side is signed: nothing can conflict
	case srcSig.unknown() || dstSig.unknown():
		fmt.Fprintln(cmd.ErrOrStderr(), "warning: --update: could not determine which key signed one or both repositories' packages; not checking that they match")
		return nil
	case sign.SameSigner(srcSig.ids, dstSig.ids):
		return nil
	}
	return fmt.Errorf("--update: the source's packages are signed by %s but the destination's by %s; "+
		"merging them would break clients that trust only one. Re-sign the packages as they are copied with --resign-packages and a signing key, or copy into a separate repository",
		srcSig.describe(), dstSig.describe())
}

// signers describes who signed a repository's packages.
type signers struct {
	ids    []string // long key ids, when they could be determined
	signed bool     // whether the packages carry a signature at all
}

// unknown reports whether the packages are signed by someone this tool could
// not identify, which is different from their being unsigned.
func (s signers) unknown() bool { return s.signed && len(s.ids) == 0 }

func (s signers) describe() string {
	switch {
	case !s.signed:
		return "no key (unsigned)"
	case len(s.ids) == 0:
		return "an unidentified key"
	default:
		return strings.Join(s.ids, ", ")
	}
}

// signerIDs reports who signed a repository's packages: the recorded key
// fingerprint when its config has one, otherwise the issuer read out of a
// single sampled package.
func signerIDs(cmd *cobra.Command, cfgKey string, r *repo.Repo, pkgs []*repodata.Package) (signers, error) {
	if cfgKey != "" {
		// The config records the primary key; gpg may have signed with any of
		// its subkeys, so all of them count as the same signer.
		return signers{ids: sign.KeyAndSubkeyIDs(cfgKey), signed: true}, nil
	}
	if len(pkgs) == 0 {
		return signers{}, nil
	}
	tmp, err := r.GetToFile(ctx(cmd), pkgs[0].Location)
	if err != nil {
		return signers{}, fmt.Errorf("sampling %s: %w", pkgs[0].Location, err)
	}
	defer os.Remove(tmp)
	ids, signed, err := sign.PackageKeyIDs(tmp)
	if err != nil {
		return signers{}, err
	}
	for i, id := range ids {
		ids[i] = normalizeKeyID(id)
	}
	return signers{ids: ids, signed: signed}, nil
}

// configKeyID returns a config's recorded signing key as a comparable long key
// id, or "" when the config records none (or records that nothing was signed).
func configKeyID(cfg *repoconfig.Config) string {
	if cfg == nil || !cfg.SignPackages || cfg.GPGKeyID == "" {
		return ""
	}
	return normalizeKeyID(cfg.GPGKeyID)
}

// applyCopyConfig records the copy's origin in the destination config and
// applies any base URL change.
func applyCopyConfig(cfg *repoconfig.Config, srcLoc string, cf *copyFlags) {
	if cf.repoName != "" {
		cfg.Name = cf.repoName
	}
	switch {
	case cf.removeBaseURL:
		cfg.BaseURL = ""
	case cf.baseURL != "":
		cfg.BaseURL = cf.baseURL
	}
	cfg.CopySource = srcLoc
}

// transformReasons lists, for the error message, the options that made this
// copy rebuild the metadata rather than replicate it.
func transformReasons(cmd *cobra.Command, run copyRun) []string {
	cf := run.cf
	var why []string
	add := func(cond bool, name string) {
		if cond {
			why = append(why, name)
		}
	}
	add(cf.update, "--update")
	add(cf.rebuildMetadata, "--rebuild-metadata")
	add(cf.resign, "--resign-packages")
	add(cf.pruneOlder, "--prune-older")
	add(cf.latestOnly, "--latest-only")
	add(len(cf.include) > 0, "--include")
	add(len(cf.exclude) > 0, "--exclude")
	add(len(cf.arches) > 0, "--arch")
	add(len(cf.kinds) > 0, "--kinds")
	add(len(cf.excludeKinds) > 0, "--exclude-kinds")
	add(run.relocate, "--location-prefix")
	add(run.changeBaseURL, "--baseurl/--remove-baseurl")
	add(cmd.Flags().Changed("sign-packages"), "--sign-packages")
	if len(why) == 0 {
		why = append(why, "the requested changes")
	}
	return why
}

func totalObjectSize(objs []repo.SourceObject) int64 {
	var total int64
	for _, o := range objs {
		if o.Size > 0 {
			total += o.Size
		}
	}
	return total
}

func deletedSuffix(stats *repo.CopyStats) string {
	if len(stats.Deleted) == 0 {
		return ""
	}
	return fmt.Sprintf(", %d extraneous file(s) deleted", len(stats.Deleted))
}
