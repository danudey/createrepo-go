package main

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/danudey/createrepo-go/pkg/repocheck"
)

// errCheckFailed signals that validation ran but reported failures. The report
// has already been printed, so main exits non-zero without printing it again.
var errCheckFailed = errors.New("repository check failed")

// checkFlags holds the check-specific options.
type checkFlags struct {
	repoFile    string
	level       string
	arch        string
	releasever  string
	packages    string
	versions    string
	concurrency int
	timeout     time.Duration
	verbose     bool
}

func checkCmd() *cobra.Command {
	var cf checkFlags
	cmd := &cobra.Command{
		Use:   "check <repo-url | repo-file | baseurl>",
		Short: "Validate a repository's metadata and packages",
		Long: `Validate a dnf/yum (rpm-md) repository against any backend (local disk,
SSH/SFTP, S3, GCS, or HTTP(S)). It reads the repository metadata, parses the
package index, and checks that the metadata is internally consistent and that
the packages it references actually exist with the size and checksum the
metadata claims.

The argument may be:
  - a repository location (a local path, or a file://, sftp://, s3://, gs:// or
    http(s):// URL), or
  - a path/URL to a .repo file (ending in ".repo"); its baseurl may contain
    $releasever/$basearch variables, supplied with --releasever and --arch.

Validation is layered; each --level includes everything the previous one does:
  metadata  repodata/repomd.xml is present and parseable, and every index file
            it references downloads with the correct size, checksum, and
            decompressed (open) size/checksum.
  head      ...plus every package in the index exists with the size the
            metadata claims (and, when the backend can checksum without
            transferring the file, the correct checksum), and every
            intra-repository dependency is satisfied by an available version.
  fetch     ...plus every package is downloaded and its size and checksum
            verified, then — if the rpm command is available — its internal
            header/payload digests are verified.

Examples:
  # Validate a live HTTP repository for RHEL 8 and 9 from a .repo file:
  createrepo-go check --releasever 8,9 \
      https://downloads.tigera.io/ee/rpms/v3.22/calico_enterprise.repo

  # Fully download and checksum-verify a local repository:
  createrepo-go check --level fetch /srv/repo

  # Only validate that an S3 repository's metadata is internally consistent:
  createrepo-go check --level metadata s3://my-bucket/el8`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			input := cf.repoFile
			if input == "" {
				if len(args) < 1 {
					return fmt.Errorf("a repo file/URL or repository location is required")
				}
				input = args[0]
			}

			level, err := repocheck.ParseLevel(cf.level)
			if err != nil {
				return err
			}

			// versions default depends on level: latest for fetch (so a full
			// download stays cheap), all for metadata/head.
			latestOnly := level == repocheck.LevelFetch
			switch strings.ToLower(strings.TrimSpace(cf.versions)) {
			case "":
				// keep level-derived default
			case "latest":
				latestOnly = true
			case versionsAll:
				latestOnly = false
			default:
				return fmt.Errorf("invalid --versions %q (want latest|all)", cf.versions)
			}

			out := cmd.OutOrStdout()
			errOut := cmd.ErrOrStderr()

			var logf func(format string, args ...any)
			if cf.verbose {
				logf = func(format string, a ...any) { fmt.Fprintf(errOut, format+"\n", a...) }
			}

			rpm := repocheck.DetectRPM()
			if level == repocheck.LevelFetch && !rpm.Available() {
				fmt.Fprintln(errOut, "warning: the 'rpm' command was not found; package payload/header digest verification will be skipped (file size and checksum are still verified)")
			}

			fmt.Fprintf(out, "check: level=%s versions=%s\n", level, versionsLabel(latestOnly))

			results, warnings, err := repocheck.Run(ctx(cmd), repocheck.Options{
				Input:       input,
				Releasevers: splitList(cf.releasever),
				Arches:      resolveArches(cf.arch),
				Packages:    splitList(cf.packages),
				Level:       level,
				LatestOnly:  latestOnly,
				Concurrency: cf.concurrency,
				Timeout:     cf.timeout,
				RPM:         rpm,
				Logf:        logf,
				OnTargetStart: func(t repocheck.Target) {
					fmt.Fprintf(out, "\n== %s ==\n   %s\n", t.Label, t.BaseURL)
				},
			})
			for _, w := range warnings {
				fmt.Fprintln(errOut, "warning:", w)
			}
			if err != nil {
				return err
			}

			if failed := reportCheck(out, results); failed {
				return errCheckFailed
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cf.repoFile, "repo-file", "", "path or URL to a .repo file (alternative to the positional argument)")
	cmd.Flags().StringVar(&cf.level, "level", "head", "validation depth: metadata|head|fetch")
	cmd.Flags().StringVar(&cf.arch, "arch", "", "comma-separated architectures to check (e.g. x86_64,aarch64). 'all' or empty means the host arch; 'any' checks every arch in the metadata")
	cmd.Flags().StringVar(&cf.releasever, "releasever", "", "comma-separated $releasever values to substitute into a .repo baseurl (e.g. 8,9)")
	cmd.Flags().StringVar(&cf.packages, "packages", "", "comma-separated package names to check (empty means all)")
	cmd.Flags().StringVar(&cf.versions, "versions", "", "which versions to check: latest|all (default: latest for fetch, all for metadata/head)")
	cmd.Flags().IntVar(&cf.concurrency, "concurrency", 6, "number of packages to check in parallel")
	cmd.Flags().DurationVar(&cf.timeout, "timeout", 60*time.Second, "per-operation timeout (0 to disable)")
	cmd.Flags().BoolVarP(&cf.verbose, "verbose", "v", false, "print a line for every check, not just failures and warnings")
	return cmd
}

// reportCheck prints failures/warnings and a summary. It returns true if at
// least one check failed.
func reportCheck(out io.Writer, results []repocheck.Result) bool {
	oks, fails, warns, skips := repocheck.Summarize(results)

	// Sort failures/warnings to the top of a detail listing.
	sorted := make([]repocheck.Result, len(results))
	copy(sorted, results)
	sort.SliceStable(sorted, func(i, j int) bool {
		return statusRank(sorted[i].Status) < statusRank(sorted[j].Status)
	})

	if fails > 0 || warns > 0 {
		fmt.Fprintf(out, "\nIssues:\n")
		for _, r := range sorted {
			if r.Status == repocheck.StatusFail || r.Status == repocheck.StatusWarn {
				fmt.Fprintf(out, "  [%s] %s :: %s\n        %s\n        %s\n", r.Status, r.Target, r.Kind, r.Detail, r.Loc)
			}
		}
	}

	fmt.Fprintf(out, "\nSummary: %d ok, %d failed, %d warnings, %d skipped\n", oks, fails, warns, skips)
	if fails > 0 {
		fmt.Fprintln(out, "RESULT: FAIL")
		return true
	}
	fmt.Fprintln(out, "RESULT: OK")
	return false
}

func statusRank(s repocheck.Status) int {
	switch s {
	case repocheck.StatusFail:
		return 0
	case repocheck.StatusWarn:
		return 1
	case repocheck.StatusSkip:
		return 2
	default:
		return 3
	}
}

// versionsAll is the --versions value meaning "every version"; archAll is the
// --arch value meaning "the host architecture only". They share a spelling but
// not a meaning.
const (
	versionsAll = "all"
	archAll     = "all"
)

func versionsLabel(latestOnly bool) string {
	if latestOnly {
		return "latest"
	}
	return versionsAll
}

// resolveArches turns the --arch flag into a concrete list. Empty or "all"
// means the host arch; "any" means do not filter by arch.
func resolveArches(flagVal string) []string {
	switch strings.ToLower(strings.TrimSpace(flagVal)) {
	case "any":
		return nil
	case "", archAll:
		return []string{hostArch()}
	default:
		return splitList(flagVal)
	}
}

// hostArch maps the Go arch to the RPM arch naming.
func hostArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "i686"
	case "arm":
		return "armv7hl"
	default:
		return runtime.GOARCH
	}
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
