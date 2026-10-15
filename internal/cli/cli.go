// Package cli holds the command-line plumbing createrepo-go and createapt-go
// share: environment handling, the progress display's wiring, prompts, and the
// check report. Anything that knows a repository format stays in the command.
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danudey/createrepo-go/pkg/checkcore"
	"github.com/danudey/createrepo-go/pkg/progress"
)

// Ctx returns the command's context, or a background one when it has none.
func Ctx(cmd *cobra.Command) context.Context {
	if c := cmd.Context(); c != nil {
		return c
	}
	return context.Background()
}

// ApplyAWSEnv translates --profile and --region into AWS_PROFILE and
// AWS_REGION, where the AWS SDK's own configuration loading picks them up. An
// empty value leaves the environment as it was.
func ApplyAWSEnv(profile, region string) {
	if profile != "" {
		_ = os.Setenv("AWS_PROFILE", profile)
	}
	if region != "" {
		_ = os.Setenv("AWS_REGION", region)
	}
}

// EnvBool reads a boolean from the environment, accepting the spellings a CI
// configuration is likely to use. An unset or unrecognised value is false.
func EnvBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// SplitList splits a comma-separated flag value, trimming each part and
// dropping empty ones.
func SplitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// SetupProgress builds the --progress display (nil when it is off) and routes
// the command's own output through it.
//
// The bars are drawn on stderr, so a run whose stdout is piped into something
// else still produces exactly the output it always did. Every other line the
// command prints has to go through the display as well: written straight to the
// terminal it would be painted over by the next redraw.
func SetupProgress(cmd *cobra.Command, enabled bool) *progress.Tracker {
	tr := progress.New(cmd.ErrOrStderr(), enabled)
	if tr != nil {
		cmd.SetOut(tr.Wrap(cmd.OutOrStdout()))
		cmd.SetErr(tr.Wrap(cmd.ErrOrStderr()))
	}
	return tr
}

// PromptYesNo asks question on the command's output and reads the answer from
// its input. Anything but y or yes, including no input at all, is no.
func PromptYesNo(cmd *cobra.Command, question string) bool {
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", question)
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// WriteConfig saves a repository's config file and says so, or says what it
// would do under --dry-run. Failing to write it is a warning, not an error:
// the repository itself has already been published.
func WriteConfig(cmd *cobra.Command, dryRun bool, path string, save func() error) {
	out := cmd.OutOrStdout()
	if dryRun {
		fmt.Fprintf(out, "[dry-run] config: would write %s\n", path)
		return
	}
	if err := save(); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not write %s: %v\n", path, err)
		return
	}
	fmt.Fprintf(out, "config: wrote %s\n", path)
}

// DeletedSuffix describes the files a pruning copy deleted, for the end of its
// summary line; nothing when there were none.
func DeletedSuffix(deleted int) string {
	if deleted == 0 {
		return ""
	}
	return fmt.Sprintf(", %d extraneous file(s) deleted", deleted)
}

// ReportCheck prints the issues and summary of a check run, returning whether
// any check failed.
func ReportCheck(out io.Writer, results []checkcore.Result) bool {
	oks, fails, warns, skips := checkcore.Summarize(results)

	// Failures, then warnings, at the top of the detail listing.
	sorted := make([]checkcore.Result, len(results))
	copy(sorted, results)
	sort.SliceStable(sorted, func(i, j int) bool {
		return statusRank(sorted[i].Status) < statusRank(sorted[j].Status)
	})

	if fails > 0 || warns > 0 {
		fmt.Fprintf(out, "\nIssues:\n")
		for _, r := range sorted {
			if r.Status == checkcore.StatusFail || r.Status == checkcore.StatusWarn {
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

func statusRank(s checkcore.Status) int {
	switch s {
	case checkcore.StatusFail:
		return 0
	case checkcore.StatusWarn:
		return 1
	case checkcore.StatusSkip:
		return 2
	default:
		return 3
	}
}
