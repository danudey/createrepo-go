package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/repoconfig"
	"github.com/danudey/createrepo-go/pkg/takeover"
)

// errTakeoverBlocked signals that the analysis found something that would break
// the repository. The report has already been printed, so main exits non-zero
// without printing anything further.
var errTakeoverBlocked = errors.New("takeover would break this repository")

// takeoverFlags holds the takeover-specific options.
type takeoverFlags struct {
	fromPackages bool
	pruneOlder   bool
	sample       int
	asJSON       bool
	adopt        bool
	repoName     string
	repoURL      string
	assumeYes    bool
}

func takeoverCmd() *cobra.Command {
	var tf takeoverFlags
	cmd := &cobra.Command{
		Use:   "takeover <repo-url>",
		Short: "Report what this tool would do to a repository it did not create",
		Long: `Analyse an existing repository and report exactly what republishing it with the
given settings would change, without transferring or writing anything.

It answers the question a takeover actually raises: this repository was created
by something else — what do I lose, what moves, and what do its clients notice,
if I start managing it with this tool?

The analysis is a real dry run. The repository is loaded, the reconciliation the
settings imply is applied to the in-memory index, and the commit plan is
computed from it, so the report describes the operation that would really run
rather than a description of one. Every flag that affects a publish affects the
report: --target, --compression, --location-prefix, --changelog-limit,
--sign-metadata/--gpg-key-id, --prune-older and --from-packages.

Findings are ranked:
  blocking  republishing as configured breaks something clients rely on
            (metadata they would lose, a signature that stops verifying, a
            transfer that cannot be carried out)
  advisory  the repository changes in a way worth knowing about first
  info      observed, changes nothing

The command exits non-zero while any blocking finding stands, so it can gate a
migration. Nothing is written unless --adopt is given, which records the
settings in ` + repoconfig.Path + ` and still publishes nothing: the republish
itself is done afterwards with rebuild.

Examples:
  # What would happen to this repository if we took it over as an EL9 repo?
  createrepo-go takeover s3://my-bucket/el9 --target rhel9

  # The same, checking the metadata against the RPMs themselves.
  createrepo-go takeover /srv/repo --from-packages

  # Gate a migration in CI.
  createrepo-go takeover https://downloads.example.com/el8 --json > report.json

  # Claim the repository: record the settings, then republish with them.
  createrepo-go takeover /srv/repo --target rhel8 --adopt --yes
  createrepo-go rebuild  /srv/repo --from-packages`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			location := args[0]
			r, prev, err := openRepo(cmd, location, false)
			if err != nil {
				return err
			}
			defer r.Close()

			opts := takeover.Options{
				Target:          gf.target,
				Compression:     gf.compression,
				ChecksumType:    gf.checksum,
				LocationPrefix:  gf.locationPrefix,
				Relocate:        cmd.Flags().Changed("location-prefix"),
				FromPackages:    readPackages(cmd, r.Backend(), tf.fromPackages),
				ChangelogLimit:  gf.changelogLimit,
				PruneOlder:      tf.pruneOlder,
				SignMetadata:    gf.signMetadata,
				SignPackages:    gf.signPackages,
				SignatureFormat: gf.signatureFormat,
				GPGKeyID:        signingKeyID(),
				SamplePackages:  tf.sample,
				Name:            tf.repoName,
				BaseURL:         tf.repoURL,
				ExistingConfig:  prev,
			}
			if !tf.asJSON {
				opts.Progress = func(line string) { fmt.Fprintln(cmd.OutOrStdout(), line) }
			}

			rep, err := takeover.Analyze(ctx(cmd), r, opts)
			if err != nil {
				return err
			}

			if tf.asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(rep); err != nil {
					return err
				}
			} else {
				printTakeoverReport(cmd.OutOrStdout(), rep)
			}

			if tf.adopt {
				if err := adoptRepository(cmd, r.Backend(), rep, tf); err != nil {
					return err
				}
			}
			if !rep.OK() {
				return errTakeoverBlocked
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&tf.fromPackages, "from-packages", false, "re-read every RPM and compare the published metadata against the file (default: on when the packages are local files, off when reading them means downloading)")
	cmd.Flags().BoolVar(&tf.pruneOlder, "prune-older", false, "report what dropping superseded versions would do")
	cmd.Flags().IntVar(&tf.sample, "sample", 1, "packages to download and inspect to find out who signed them (0 to skip)")
	cmd.Flags().BoolVar(&tf.asJSON, "json", false, "emit the report as JSON")
	cmd.Flags().BoolVar(&tf.adopt, "adopt", false, "record the settings in "+repoconfig.Path+" (publishes nothing; republish with rebuild)")
	cmd.Flags().StringVar(&tf.repoName, "repo-name", "", "human-readable repository name to record in the config file")
	cmd.Flags().StringVar(&tf.repoURL, "repo-url", "", "public base URL end users fetch the repository from, recorded in the config file")
	cmd.Flags().BoolVarP(&tf.assumeYes, "yes", "y", false, "skip the confirmation prompt for --adopt")
	return cmd
}

// readPackages decides whether to re-read the RPMs, following rebuild's rule:
// free when the packages are local files, so on by default there, and off
// elsewhere because it means downloading the whole repository.
func readPackages(cmd *cobra.Command, be backend.Backend, requested bool) bool {
	if cmd.Flags().Changed("from-packages") {
		return requested
	}
	return backend.IsLocal(be)
}

// signingKeyID reports the key a republish would sign with, however it was
// supplied. A key file is named as given; the analysis only needs something it
// can compare and print.
func signingKeyID() string {
	if gf.gpgKeyID != "" {
		return gf.gpgKeyID
	}
	return gf.gpgKey
}

// adoptRepository records the analysed settings in the repository's config
// file. It is the whole of what --adopt does: nothing about the published
// metadata changes, so the repository keeps serving exactly what it served
// before, and a later rebuild publishes with the recorded settings.
func adoptRepository(cmd *cobra.Command, be backend.Backend, rep *takeover.Report, tf takeoverFlags) error {
	out := cmd.OutOrStdout()
	if !rep.OK() {
		fmt.Fprintf(out, "\n%d blocking finding(s) stand; --adopt records the settings anyway (it publishes nothing), "+
			"but do not rebuild until they are resolved.\n", rep.Count(takeover.Blocking))
	}
	if !tf.assumeYes && !gf.dryRun {
		if !promptYesNo(cmd, fmt.Sprintf("Record these settings in %s?", repoconfig.Path)) {
			fmt.Fprintln(out, "aborted; nothing written")
			return nil
		}
	}
	cfg := rep.Config
	writeRepoConfig(cmd, be, &cfg)
	return nil
}

// printTakeoverReport renders the analysis. Sections with nothing to say are
// omitted, so a clean repository produces a short report.
func printTakeoverReport(out io.Writer, rep *takeover.Report) {
	fmt.Fprintf(out, "\nTakeover analysis for %s\n", rep.Location)
	printPublished(out, rep)
	printProposed(out, rep)
	printChanges(out, rep)
	printDiff(out, rep)
	printFindings(out, rep)
	printVerdict(out, rep)
}

// field renders one "  label:  value" line at a fixed width.
func field(out io.Writer, label, format string, args ...any) {
	fmt.Fprintf(out, "  %-20s %s\n", label+":", fmt.Sprintf(format, args...))
}

func printPublished(out io.Writer, rep *takeover.Report) {
	p := rep.Published
	fmt.Fprintln(out, "\nAs published")
	field(out, "packages", "%d (%s)", p.Packages, humanBytes(p.PackageBytes))
	scope := ""
	if !p.Listed {
		scope = " (metadata only; this backend cannot list its contents)"
	}
	field(out, "repository", "%s across %d object(s)%s", humanBytes(p.TotalBytes), p.TotalFiles, scope)

	var types []string
	for _, m := range p.Metadata {
		types = append(types, m.Type)
	}
	field(out, "metadata", "%s [%s]", strings.Join(types, ", "), strings.Join(p.Compressions, ", "))
	field(out, "package checksums", "%s", strings.Join(p.ChecksumTypes, ", "))
	if len(p.PackageDirs) > 0 {
		field(out, "packages live in", "%s", strings.Join(p.PackageDirs, ", "))
	}
	switch {
	case p.SignedMetadata && len(p.MetadataSigners) > 0:
		field(out, "repomd.xml", "signed by %s", strings.Join(p.MetadataSigners, ", "))
	case p.SignedMetadata:
		field(out, "repomd.xml", "signed by an unidentified key")
	default:
		field(out, "repomd.xml", "unsigned")
	}
	if p.SampledPackages > 0 {
		signers := "no key (unsigned)"
		if len(p.PackageSigners) > 0 {
			signers = strings.Join(p.PackageSigners, ", ")
		}
		field(out, "packages signed by", "%s (from %d sampled)", signers, p.SampledPackages)
	}
	managed := "absent — this repository has not been published by this tool"
	if rep.Managed {
		managed = "present — this repository is already managed by this tool"
	}
	field(out, repoconfig.Path, "%s", managed)
}

func printProposed(out io.Writer, rep *takeover.Report) {
	p, pub := rep.Proposed, rep.Published
	fmt.Fprintln(out, "\nSettings a republish would use")
	if p.Target != "" {
		field(out, "target", "%s", p.Target)
	}
	from := strings.Join(pub.Compressions, "+")
	if from == p.Compression {
		field(out, "compression", "%s (unchanged)", p.Compression)
	} else {
		field(out, "compression", "%s -> %s", from, p.Compression)
	}
	switch {
	case p.Relocate:
		field(out, "location prefix", "%s (existing packages move there)", orRoot(p.LocationPrefix))
	default:
		field(out, "location prefix", "%s (only for packages added later; existing packages stay put)", orRoot(p.LocationPrefix))
	}
	if p.FromPackages {
		field(out, "re-read packages", "yes — the metadata is checked against the RPMs themselves")
	} else {
		field(out, "re-read packages", "no — the published metadata is taken at its word (--from-packages)")
	}
	field(out, "changelogs", "%s", changelogSetting(p))
	field(out, "metadata signing", "%s", signingSetting(p.SignMetadata, p.GPGKeyID))
	field(out, "package signing", "%s", signingSetting(p.SignPackages, p.GPGKeyID))
}

// changelogSetting describes the changelog limit, which only bites when the
// packages are re-read.
func changelogSetting(p takeover.Proposed) string {
	switch {
	case !p.FromPackages:
		return "kept as published (only re-reading the packages re-derives them)"
	case p.ChangelogLimit == 0:
		return "every entry kept"
	default:
		return fmt.Sprintf("newest %d entries per package", p.ChangelogLimit)
	}
}

func signingSetting(on bool, key string) string {
	if !on {
		return "off"
	}
	if key == "" {
		return "on (no key given)"
	}
	return "on, with " + key
}

// orRoot names an empty location prefix for a human reader.
func orRoot(prefix string) string {
	if strings.Trim(prefix, "/") == "" {
		return "(repository root)"
	}
	return strings.Trim(prefix, "/") + "/"
}

func printChanges(out io.Writer, rep *takeover.Report) {
	c := rep.Changes
	fmt.Fprintln(out, "\nWhat a republish would do")
	field(out, "packages", "%d -> %d (%+d)", c.PackagesBefore, c.PackagesAfter, c.PackagesAfter-c.PackagesBefore)
	if c.Corrected > 0 {
		field(out, "metadata corrected", "%d package(s) whose file did not match the metadata", c.Corrected)
	}
	if len(c.MetadataWritten) > 0 {
		field(out, "write", "%d metadata file(s)", len(c.MetadataWritten))
	}
	if c.SignMetadata {
		field(out, "sign", "repodata/repomd.xml.asc")
	}
	if c.ServerSideMoves > 0 {
		field(out, "move", "%d package(s), server-side (no re-upload)", c.ServerSideMoves)
	}
	if c.Uploads > 0 {
		field(out, "upload", "%d file(s) (%s)", c.Uploads, humanBytes(c.UploadBytes))
	}
	if len(c.DeletedFiles) > 0 {
		field(out, "delete", "%d file(s) (%s)", len(c.DeletedFiles), humanBytes(c.DeletedBytes))
		const shown = 10
		for i, f := range c.DeletedFiles {
			if i == shown {
				fmt.Fprintf(out, "      … and %d more (--json lists them all)\n", len(c.DeletedFiles)-shown)
				break
			}
			fmt.Fprintf(out, "      - %s\n", f)
		}
	}
}

func printDiff(out io.Writer, rep *takeover.Report) {
	if len(rep.Diff) == 0 {
		if rep.Changes.PackagesBefore > 0 {
			fmt.Fprintln(out, "\nEvery package's metadata would be republished unchanged, field for field.")
		}
		return
	}
	fmt.Fprintln(out, "\nPackage metadata that would change (published vs regenerated)")
	for _, d := range rep.Diff {
		fmt.Fprintf(out, "  %-24s %5d package(s)   e.g. %s\n", d.Field, d.Packages, strings.Join(d.Examples, ", "))
	}
}

func printFindings(out io.Writer, rep *takeover.Report) {
	if len(rep.Findings) == 0 {
		return
	}
	fmt.Fprintln(out, "\nFindings")
	for _, f := range rep.Findings {
		fmt.Fprintf(out, "  %-8s %s: %s\n", strings.ToUpper(string(f.Severity)), f.Code, f.Summary)
		if f.Detail != "" {
			fmt.Fprintf(out, "           %s\n", wrap(f.Detail, 11))
		}
		for _, item := range f.Items {
			fmt.Fprintf(out, "           - %s\n", item)
		}
		if f.Fix != "" {
			fmt.Fprintf(out, "           fix: %s\n", wrap(f.Fix, 11))
		}
	}
}

func printVerdict(out io.Writer, rep *takeover.Report) {
	blocking := rep.Count(takeover.Blocking)
	verdict := "safe to republish with these settings"
	if blocking > 0 {
		verdict = "not safe to republish with these settings"
	}
	fmt.Fprintf(out, "\nVerdict: %d blocking, %d advisory, %d info — %s.\n",
		blocking, rep.Count(takeover.Advisory), rep.Count(takeover.Info), verdict)
}

// wrap folds long text at a readable width, indenting continuation lines to
// indent columns so a finding's body stays aligned under its heading.
func wrap(text string, indent int) string {
	const width = 78
	var b strings.Builder
	col := indent
	for i, word := range strings.Fields(text) {
		if i > 0 {
			if col+1+len(word) > width {
				b.WriteString("\n")
				b.WriteString(strings.Repeat(" ", indent))
				col = indent
			} else {
				b.WriteString(" ")
				col++
			}
		}
		b.WriteString(word)
		col += len(word)
	}
	return b.String()
}
