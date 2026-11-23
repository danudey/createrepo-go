package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/danudey/createrepo-go/internal/cli"
	"github.com/danudey/createrepo-go/pkg/apt/aptdata"
	"github.com/danudey/createrepo-go/pkg/apt/repo"
	"github.com/danudey/createrepo-go/pkg/apt/repoconfig"
	"github.com/danudey/createrepo-go/pkg/apt/sources"
	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/clientfile"
	"github.com/danudey/createrepo-go/pkg/progress"
)

// versionsAll and versionsLatest are the --versions values.
const (
	versionsAll    = "all"
	versionsLatest = "latest"
)

// verifyFlags are verify's own flags.
type verifyFlags struct {
	checksums           bool
	concurrency         int
	metadataOnly        bool
	skipPackageContents bool
	arches              []string
	names               []string
	latestOnly          bool
	timeout             time.Duration
	verbose             bool
}

func verifyCmd() *cobra.Command {
	var vf verifyFlags
	var versions string
	cmd := &cobra.Command{
		Use:   "verify <repo-url | sources-file>",
		Short: "Check a repository end to end: Release, indexes, packages, signature",
		Long: `Check a suite as apt reads it, end to end, and report every defect found.
Nothing is modified.

  Release   InRelease (or Release) is present, parses, and has not passed its
            Valid-Until. Its signature is checked against --keyring or the key
            the repository's config records: InRelease and Release.gpg are both
            verified when present, and InRelease must sign the same text as
            Release. With no key, a missing or unverifiable signature is a
            warning; --verify-sigs makes it a problem.
  indexes   every file the Release lists is present with the recorded size
            and every recorded checksum, decompresses, and (Packages, Sources)
            parses. One compressed form of an index may be absent while
            another is sound, since apt fetches only one. With Acquire-By-Hash,
            the by-hash copy apt fetches must be present.
  packages  every file the indexes reference is present with the recorded size
            and checksum, and each .deb's control file names the package,
            version and architecture its index entry does.
  depends   every intra-repository dependency is satisfied.

How far the checksum check goes depends on the backend:

  local disk, SSH/SFTP  the checksum is computed from the stored file (over SSH
                        with sha256sum, so the package never crosses the
                        network), so contents are always proved.
  S3, GCS               the object store reports the checksum recorded when the
                        object was uploaded. That catches indexes and objects
                        disagreeing, but not an object whose content changed
                        afterwards.
  HTTP(S)               no checksum is available without downloading.

Pass --checksums to prove every file's checksum from its content regardless,
downloading each one the backend cannot hash in place. The summary always says
which of the three applied, so an unproved checksum is never reported as though
it had been verified.

The argument is a repository location (a local path, or a file://, sftp://,
s3://, gs:// or http(s):// URL) or a sources file (a path or URL ending in
.list or .sources), in which case every entry it names is verified as its own
repository. --suite and --component take comma-separated lists here.

  createapt-go verify https://deb.example.com/apt --suite bookworm
  createapt-go verify /etc/apt/sources.list.d/example.sources --checksums
  createapt-go verify s3://my-bucket/apt --packages hello,libfoo --versions latest`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch strings.ToLower(versions) {
			case versionsAll:
			case versionsLatest:
				vf.latestOnly = true
			default:
				return fmt.Errorf("invalid --versions %q (want %s or %s)", versions, versionsAll, versionsLatest)
			}
			return runVerify(cmd, args[0], vf)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&vf.checksums, "checksums", false, "prove every file's checksum from its content, downloading each one the backend cannot hash in place")
	f.IntVar(&vf.concurrency, "concurrency", 6, "number of packages to check in parallel")
	f.BoolVar(&vf.metadataOnly, "metadata-only", false, "check the metadata alone (Release, every index, the signature, dependencies), not the package files")
	f.BoolVar(&vf.skipPackageContents, "skip-package-contents", false, "do not open the .deb files to check their control files")
	f.StringSliceVar(&vf.arches, "arch", nil, `architectures to check, comma-separated ("all" packages count as every one; name "source" for source packages; default: every architecture)`)
	f.StringSliceVar(&vf.names, "packages", nil, "package names to check, comma-separated (default: all)")
	f.StringVar(&versions, "versions", versionsAll, "which versions of each package to check: all or latest")
	f.DurationVar(&vf.timeout, "timeout", 60*time.Second, "give up on a backend operation that makes no progress for this long (0 to disable); a download that keeps making progress is never cut off")
	f.BoolVarP(&vf.verbose, "verbose", "v", false, "print a line for every index and package checked, not just problems")
	return cmd
}

// verifyTarget is one suite of one repository to verify.
type verifyTarget struct {
	location   string
	suite      string
	components []string
	// label prefixes its output when there is more than one target.
	label string
	// arches narrows vf.arches to what a sources entry subscribes to.
	arches []string
}

func runVerify(cmd *cobra.Command, input string, vf verifyFlags) error {
	ctx := cli.Ctx(cmd)
	fl := cmd.Flags()
	suites := cli.SplitList(gf.suite)
	var components []string
	if fl.Changed("component") {
		components = cli.SplitList(gf.component)
	}

	var targets []verifyTarget
	if sources.IsFile(input) {
		entries, warnings, err := sources.Load(input, clientfile.FetchOverHTTP(ctx, vf.timeout, "createapt-go/"+version))
		for _, w := range warnings {
			fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			// A deb-src entry names the indexes its deb entry does, plus
			// Sources; verifying the suite once covers both.
			if e.Source && hasBinaryEntryFor(entries, e) {
				continue
			}
			if len(suites) > 0 && !contains(suites, e.Suite) {
				continue
			}
			if err := clientfile.CheckTargetScheme(input, e.URI); err != nil {
				return err
			}
			targets = append(targets, verifyTarget{
				location: strings.TrimRight(e.URI, "/"), suite: e.Suite,
				components: mergeComponents(e.Components, components),
				label:      "[" + e.Label() + "] ", arches: e.Arches,
			})
		}
		if len(targets) == 0 {
			return fmt.Errorf("no entries in %s match the requested suites", input)
		}
	} else {
		if len(suites) == 0 {
			suites = []string{""} // the recorded suite, resolved per target
		}
		for _, s := range suites {
			targets = append(targets, verifyTarget{location: input, suite: s, components: components})
		}
		if len(targets) > 1 {
			for i := range targets {
				targets[i].label = "[" + targets[i].suite + "] "
			}
		}
	}

	if len(targets) == 1 {
		return verifyOne(cmd, targets[0], vf)
	}
	failed := 0
	for _, t := range targets {
		if err := verifyOne(cmd, t, vf); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "%s%v\n", t.label, err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d suite(s) failed verification", failed, len(targets))
	}
	return nil
}

// verifyOne verifies one suite. The metadata is checked first and on its own,
// so a suite whose indexes are too broken to load still has every defect in
// it reported.
func verifyOne(cmd *cobra.Command, t verifyTarget, vf verifyFlags) error {
	ctx := cli.Ctx(cmd)
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	be, err := backend.Open(ctx, t.location)
	if err != nil {
		return err
	}
	be = backend.WithTimeout(be, vf.timeout)
	defer closeBackend(be)

	cfg, err := repoconfig.Load(ctx, be)
	if err != nil {
		return err
	}
	if err := applyConfigDefaults(cmd, cfg); err != nil {
		return err
	}
	gf.signRelease = false
	suite := t.suite
	if suite == "" {
		suite = effectiveSuite()
	}
	if err := aptdata.ValidSuite(suite); err != nil {
		return err
	}

	meta := repo.CheckMetadata(ctx, be, suite, t.components)
	if vf.verbose {
		for _, f := range meta.Files {
			status := "ok  "
			if !f.OK {
				status = "FAIL"
			}
			fmt.Fprintf(out, "%s%s %s: %s\n", t.label, status, f.Path, f.Detail)
		}
	}
	var problems []string
	report := func(warnings []repo.Problem) int {
		for _, p := range warnings {
			fmt.Fprintln(errOut, t.label+"WARNING:", p)
		}
		for _, p := range problems {
			fmt.Fprintln(errOut, t.label+"PROBLEM:", p)
		}
		return len(problems)
	}
	for _, p := range meta.Problems {
		problems = append(problems, p.String())
	}
	if !meta.Usable() {
		report(meta.Warnings)
		return fmt.Errorf("%d problem(s) found; the indexes cannot be read, so no package was checked", len(problems))
	}

	opts, err := repoOptions(false)
	if err != nil {
		return err
	}
	opts.Suite, opts.MetadataCache = suite, meta.Cache
	r, err := repo.OpenWith(ctx, be, opts)
	if err != nil {
		problems = append(problems, "the indexes cannot be loaded: "+err.Error())
		report(meta.Warnings)
		return fmt.Errorf("%d problem(s) found", len(problems))
	}

	// The Release signature is what makes the rest trustworthy. A bad one is
	// reported with the other problems rather than stopping the run, so one
	// pass shows everything that is wrong.
	keyrings, releaseKeys := verificationKeyrings(cmd, cfg)
	defer releaseKeys()
	if _, sigErr := verifyReleaseSignature(cmd, r, keyrings, "the"); sigErr != nil {
		problems = append(problems, "release signature: "+sigErr.Error())
	}

	arches := vf.arches
	if len(arches) == 0 {
		arches = t.arches
	}
	vopt := repo.VerifyOptions{
		Concurrency: vf.concurrency, Tracker: prog,
		SkipPackages: vf.metadataOnly, SkipPackageContents: vf.skipPackageContents,
		Names: vf.names, Arches: arches, Components: t.components, LatestOnly: vf.latestOnly,
	}
	if vf.verbose {
		vopt.Progress = func(e aptdata.Entry, n int) {
			status := "ok  "
			if n > 0 {
				status = "FAIL"
			}
			fmt.Fprintf(out, "%s%s %s\n", t.label, status, e.ID3())
		}
	}
	if vf.checksums && !vf.metadataOnly {
		vopt.Checksums = repo.ChecksumContent
		// A full content check can be a large transfer; say so before
		// starting rather than appearing to hang.
		if n, size := r.DownloadEstimate(vopt); n > 0 {
			fmt.Fprintf(out, "%shashing %d file(s) from %s: ~%s to download\n", t.label, n, r.Backend(), progress.Bytes(size))
		}
	}
	res, err := r.Verify(ctx, vopt)
	if err != nil {
		return err
	}
	for _, p := range res.Problems {
		problems = append(problems, p.String())
	}
	if n := report(meta.Warnings); n > 0 {
		return fmt.Errorf("%d problem(s) found", n)
	}
	fmt.Fprintln(out, t.label+verifySummary(res, meta, vf))
	return nil
}

// verifySummary renders a clean verification result, stating exactly what was
// established, so the reader is never left assuming more was proved than
// actually was.
func verifySummary(res *repo.VerifyResult, meta *repo.MetadataReport, vf verifyFlags) string {
	checked := 0
	for _, f := range meta.Files {
		if !f.Absent {
			checked++
		}
	}
	metaLine := fmt.Sprintf("%s and %d index file(s) intact", meta.ReleasePath, checked)
	subject := fmt.Sprintf("%d package(s)", res.Packages)
	if res.Sources > 0 {
		subject += fmt.Sprintf(" and %d source package(s)", res.Sources)
	}
	if vf.metadataOnly {
		return fmt.Sprintf("OK: %s; %s indexed, not checked (--metadata-only); dependencies satisfied", metaLine, subject)
	}
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
	summary := fmt.Sprintf("OK: %s\npackages: %s present with the expected size (%d file(s))", metaLine, subject, res.Files)
	if len(parts) > 0 {
		summary += "; checksums: " + strings.Join(parts, ", ")
	}
	summary += "; dependencies satisfied"
	if !vf.skipPackageContents {
		summary += "\ncontents: every .deb's control file matches its index entry"
	}
	if res.ChecksumUnconfirmed > 0 {
		summary += "\nRun with --checksums to download and hash the files whose checksum this backend cannot confirm."
	}
	return summary
}

// hasBinaryEntryFor reports whether a deb entry already covers the suite a
// deb-src entry names.
func hasBinaryEntryFor(entries []sources.Entry, src sources.Entry) bool {
	for _, e := range entries {
		if !e.Source && e.URI == src.URI && e.Suite == src.Suite {
			return true
		}
	}
	return false
}

// mergeComponents narrows an entry's components by --component, keeping the
// entry's own list when none was given.
func mergeComponents(entry, requested []string) []string {
	if len(requested) == 0 {
		return entry
	}
	if len(entry) == 0 {
		return requested
	}
	var out []string
	for _, c := range entry {
		if contains(requested, c) {
			out = append(out, c)
		}
	}
	return out
}

func contains(list []string, v string) bool { return slices.Contains(list, v) }
