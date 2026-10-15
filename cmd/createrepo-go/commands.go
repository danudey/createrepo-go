package main

import (
	"fmt"

	"github.com/danudey/createrepo-go/internal/cli"

	"github.com/spf13/cobra"

	"github.com/danudey/createrepo-go/pkg/rpm/repo"
	"github.com/danudey/createrepo-go/pkg/rpm/repodata"
)

func createCmd() *cobra.Command {
	var repoName, repoURL string
	cmd := &cobra.Command{
		Use:   "create <repo-url>",
		Short: "Initialize an empty repository (writes repodata/)",
		Long: `Initialize an empty repository (writes repodata/) and record a
createrepo-go.json config file alongside it holding the repository's metadata
(--repo-name, --repo-url) and the signing defaults in effect. Later add/remove
operations reuse those signing defaults unless overridden.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, prev, err := openRepo(cmd, args[0], true)
			if err != nil {
				return err
			}
			defer r.Close()
			plan, err := r.Commit(cli.Ctx(cmd))
			if err != nil {
				return err
			}
			printPlan(cmd, plan, gf.dryRun)
			saveRepoConfig(cmd, r, prev, repoName, repoURL)
			return nil
		},
	}
	cmd.Flags().StringVar(&repoName, "repo-name", "", "human-readable repository name to record in the config file")
	cmd.Flags().StringVar(&repoURL, "repo-url", "", "public base URL end users fetch the repository from, recorded in the config file")
	return cmd
}

func removeCmd() *cobra.Command {
	var arch, evr string
	cmd := &cobra.Command{
		Use:   "remove <repo-url> <name>...",
		Short: "Remove packages from a repository by name",
		Long: `Remove every package matching the given name(s) from the metadata.
Constrain with --arch and/or --evr (epoch:version-release). Once a package is no
longer referenced by the metadata, its RPM file is garbage-collected on publish.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			location, names := args[0], args[1:]
			r, prev, err := openRepo(cmd, location, false)
			if err != nil {
				return err
			}
			defer r.Close()

			var total int
			for _, n := range names {
				removed := r.Remove(n, arch, evr)
				for _, p := range removed {
					fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", p.NEVRA())
				}
				total += len(removed)
			}
			if total == 0 {
				return fmt.Errorf("no matching packages found")
			}
			plan, err := r.Commit(cli.Ctx(cmd))
			if err != nil {
				return err
			}
			printPlan(cmd, plan, gf.dryRun)
			saveRepoConfig(cmd, r, prev, "", "")
			return nil
		},
	}
	cmd.Flags().StringVar(&arch, "arch", "", "only remove packages of this architecture")
	cmd.Flags().StringVar(&evr, "evr", "", "only remove this epoch:version-release")
	deprecateDeleteRemoved(cmd)
	return cmd
}

func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <repo-url>",
		Short: "List packages in a repository",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := repoOptions(false)
			if err != nil {
				return err
			}
			r, err := repo.Open(cli.Ctx(cmd), args[0], opts)
			if err != nil {
				return err
			}
			defer r.Close()
			pkgs := r.Index().Packages()
			for _, p := range pkgs {
				fmt.Fprintf(cmd.OutOrStdout(), "%-40s %12d  %s\n", p.NEVRA(), p.SizePackage, p.Location)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d package(s)\n", len(pkgs))
			return nil
		},
	}
}

// printPlan renders a commit Plan to the command's output.
// dryRunPrefix marks every line of output that describes an action the run
// did not actually take.
const dryRunPrefix = "[dry-run] "

func printPlan(cmd *cobra.Command, plan *repo.Plan, dryRun bool) {
	out := cmd.OutOrStdout()
	prefix := ""
	if dryRun {
		prefix = dryRunPrefix
	}
	for _, a := range plan.Uploads {
		fmt.Fprintf(out, "%supload  %s (%d bytes) — %s\n", prefix, a.Location, a.Size, a.Reason)
	}
	for _, a := range plan.Skipped {
		// A package the copy command already transferred was reported as it
		// went past; repeating it here would double every line of a mirror's
		// output. It still counts towards the summary below.
		if a.Reason == repo.ReasonCopied {
			continue
		}
		fmt.Fprintf(out, "%sskip    %s — %s\n", prefix, a.Location, a.Reason)
	}
	// A relocation's source is also listed in DeletedRPMs (it is removed after
	// the copy); render it as a single "move" line and suppress the plain
	// "delete" line for it.
	movedFrom := make(map[string]bool, len(plan.Copies))
	for _, c := range plan.Copies {
		fmt.Fprintf(out, "%smove    %s -> %s (server-side, no upload)\n", prefix, c.From, c.To)
		movedFrom[c.From] = true
	}
	for _, href := range plan.DeletedRPMs {
		if movedFrom[href] {
			continue
		}
		fmt.Fprintf(out, "%sdelete  %s\n", prefix, href)
	}
	for _, href := range plan.ObsoleteMeta {
		fmt.Fprintf(out, "%sgc      %s\n", prefix, href)
	}
	signed := ""
	if plan.Signed {
		signed = ", repomd.xml signed"
	}
	moved := ""
	if len(plan.Copies) > 0 {
		moved = fmt.Sprintf(", %d relocated", len(plan.Copies))
	}
	fmt.Fprintf(out, "%s%d package(s); %d upload(s) (%d bytes), %d skipped%s%s\n",
		prefix, plan.Packages, len(plan.Uploads), plan.BytesToUpload, len(plan.Skipped), moved, signed)
}

// printPruneWarnings reports dependency breakages from a prune. kept lists
// versions retained because a dependent needs them (the default); broken lists
// versions dropped despite a dependent (--prune-break-deps). Warnings go to
// stderr so they stand out from the normal plan output.
func printPruneWarnings(cmd *cobra.Command, kept, broken []repodata.Breakage) {
	errOut := cmd.ErrOrStderr()
	for _, b := range kept {
		fmt.Fprintf(errOut, "warning: keeping %s: it is required by %s (%s)\n",
			b.Provider.NEVRA(), b.Dependent.NEVRA(), b.Requires.Constraint())
	}
	for _, b := range broken {
		fmt.Fprintf(errOut, "warning: pruned %s though %s requires %s (--prune-break-deps)\n",
			b.Provider.NEVRA(), b.Dependent.NEVRA(), b.Requires.Constraint())
	}
}
