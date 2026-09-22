package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/progress"
	"github.com/danudey/createrepo-go/pkg/repo"
	"github.com/danudey/createrepo-go/pkg/repodata"
)

func rebuildCmd() *cobra.Command {
	var pruneOlder, resign, assumeYes, fromPackages bool
	cmd := &cobra.Command{
		Use:   "rebuild <repo-url>",
		Short: "Reconcile an existing repository against updated options",
		Long: `Load an existing repository's metadata, apply updated options, reconcile the
new state, and republish.

When the packages are local files, rebuild re-reads every RPM and regenerates
its metadata from the file, so a package that was replaced under the same name
stops being described by the checksum, size and dependencies of the build it
superseded. Reading them is free in that case, so it is the default. When the
packages live on remote storage, reading them means downloading the repository,
so rebuild does not do it unless asked with --from-packages — and says clearly
that it is republishing the metadata it was given rather than checking it.

Apart from that, rebuild never downloads RPMs: it can prune superseded versions
(--prune-older), move every package under a new --location-prefix (relocated
server-side, no re-upload), and re-sign repomd.xml when a signing key is
configured.

Opt-in extras:
  --resign-packages          re-sign every RPM with the signing key. This
                             downloads the whole repository; rebuild reports the
                             download size and asks for confirmation first.
  --remove-unreferenced-rpms delete .rpm files the metadata no longer references.
  --remove-stale-metadata    delete repodata files no longer in use.

rebuild always prints a before/after summary and asks for confirmation before
making changes. Pass --yes to skip the prompt, or --dry-run to only report.
The cleanup flags require a backend that can list its contents (not plain HTTP).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			location := args[0]
			r, prev, err := openRepo(cmd, location, false)
			if err != nil {
				return err
			}
			defer r.Close()

			// Snapshot what was published before anything is reconciled, so the
			// report contrasts it with the result — including any correction
			// the refresh below makes.
			beforePkgs := r.Index().Packages()
			beforeCount := len(beforePkgs)
			var beforePkgBytes int64
			for _, p := range beforePkgs {
				beforePkgBytes += p.SizePackage
			}

			// Re-read the RPMs before reconciling, so the prune's version
			// comparisons and every number below describe what is actually
			// stored rather than what the old metadata claimed.
			refreshed, err := refreshFromPackages(cmd, r, fromPackages)
			if err != nil {
				return err
			}
			totalBytes, totalFiles, listed, err := r.TotalSize(ctx(cmd))
			if err != nil {
				return err
			}

			// Reconcile the index in memory — no downloads.
			var pruneRep repo.PruneReport
			if pruneOlder {
				pruneRep = r.PruneOlderVersions()
				printPruneWarnings(cmd, pruneRep.Kept, pruneRep.Broken)
			}
			relocated := 0
			if cmd.Flags().Changed("location-prefix") {
				relocated = r.RelocateAll(gf.locationPrefix)
			}

			// The re-sign download volume is known from the metadata before any
			// RPM is fetched, so we can warn accurately up front.
			var resignCount int
			var resignBytes int64
			if resign {
				for _, p := range r.ResignTargets() {
					resignCount++
					resignBytes += p.SizePackage
				}
			}

			// Preview the plan (no side effects), including directory cleanups.
			preview, err := r.Plan(ctx(cmd))
			if err != nil {
				return err
			}

			afterPkgs := r.Index().Packages()
			var afterPkgBytes int64
			for _, p := range afterPkgs {
				afterPkgBytes += p.SizePackage
			}

			printRebuildReport(cmd, rebuildReport{
				location:       location,
				beforeCount:    beforeCount,
				afterCount:     len(afterPkgs),
				beforePkgBytes: beforePkgBytes,
				afterPkgBytes:  afterPkgBytes,
				totalBytes:     totalBytes,
				totalFiles:     totalFiles,
				sizeListed:     listed,
				relocated:      relocated,
				resignCount:    resignCount,
				resignBytes:    resignBytes,
				refreshed:      refreshed,
				freedBytes:     deletedBytes(beforePkgs, preview),
				plan:           preview,
				depProblems:    r.CheckDependencies(),
			})

			if gf.dryRun {
				printPlan(cmd, preview, true)
				return nil
			}

			// Destructive from here on (deletes and possibly a large download):
			// confirm unless the operator opted out.
			if !assumeYes {
				if !promptYesNo(cmd, "Proceed with these changes?") {
					fmt.Fprintln(stdout(cmd), "aborted; no changes made")
					return nil
				}
			}

			// Re-sign after approval — the download-heavy step.
			if resign {
				cleanup, err := resignPackages(cmd, r)
				if err != nil {
					return err
				}
				defer cleanup()
			}

			plan, err := r.Commit(ctx(cmd))
			if err != nil {
				return err
			}
			printPlan(cmd, plan, false)
			saveRepoConfig(cmd, r, prev, "", "")
			return nil
		},
	}
	cmd.Flags().BoolVar(&fromPackages, "from-packages", false, "re-read every RPM and regenerate its metadata from the file (default: on when the packages are local files, off when reading them means downloading)")
	cmd.Flags().BoolVar(&pruneOlder, "prune-older", false, "drop superseded versions, keeping only the newest of each name+arch")
	cmd.Flags().BoolVar(&gf.pruneBreakDeps, "prune-break-deps", false, "when pruning, drop a version even if another package depends on it (default: keep it and warn)")
	cmd.Flags().BoolVar(&resign, "resign-packages", false, "re-sign every RPM with the signing key (downloads the whole repository)")
	cmd.Flags().BoolVar(&gf.removeUnreferencedRPMs, "remove-unreferenced-rpms", false, "delete .rpm files the metadata no longer references")
	cmd.Flags().BoolVar(&gf.removeStaleMetadata, "remove-stale-metadata", false, "delete repodata files no longer in use")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

// refreshFromPackages re-derives every package's metadata from its RPM file,
// which is what makes a rebuild correct a repository whose package files were
// replaced underneath the metadata. It is on by default when the packages are
// local files, because re-reading them costs nothing; when reading them means
// downloading the whole repository it is off unless asked for, and rebuild says
// plainly that it is publishing the metadata it was given rather than checking
// it. It returns nil when no refresh was performed.
func refreshFromPackages(cmd *cobra.Command, r *repo.Repo, requested bool) (*repo.RefreshResult, error) {
	out, errOut := stdout(cmd), stderr(cmd)
	local := backend.IsLocal(r.Backend())
	refresh := local
	if cmd.Flags().Changed("from-packages") {
		refresh = requested
	}

	if !refresh {
		fmt.Fprintf(errOut, "warning: the metadata is being regenerated from the published index, not re-read from the RPM files, "+
			"so anything the metadata gets wrong about a package (checksum, size, dependencies) stays wrong. "+
			"Pass --from-packages to re-read them")
		if n, size := r.RefreshEstimate(); n > 0 {
			fmt.Fprintf(errOut, ", which downloads %d package(s) (~%s)", n, humanBytes(size))
		}
		fmt.Fprintln(errOut, ".")
		return nil, nil
	}

	if n, size := r.RefreshEstimate(); n > 0 {
		fmt.Fprintf(out, "re-reading %d package(s) from %s: ~%s to download\n", n, r.Backend(), humanBytes(size))
	}
	res, err := r.RefreshFromPackages(ctx(cmd), repo.RefreshOptions{
		Progress: func(p *repodata.Package, changed bool) {
			if changed {
				fmt.Fprintf(out, "refreshed %s from its RPM (the published metadata did not match the file)\n", p.NEVRA())
			}
		},
	})
	if err != nil {
		return nil, err
	}
	for _, loc := range res.Missing {
		fmt.Fprintf(errOut, "warning: %s is referenced by the metadata but not present; its entry is left as published\n", loc)
	}
	for _, bad := range res.Unreadable {
		fmt.Fprintf(errOut, "warning: could not read %s as an RPM; its entry is left as published\n", bad)
	}
	for _, c := range res.Conflicts {
		fmt.Fprintf(errOut, "warning: %s; the metadata can only name one of them, so both entries are left as published. "+
			"Remove one of the files if the duplication is unintended\n", c)
	}
	return res, nil
}

// resignPackages downloads, re-signs and re-indexes every package in the
// repository, staging each re-signed RPM for upload. It returns a cleanup that
// removes the temporary signed files; the caller must defer it until after the
// commit that uploads them. Re-signing overwrites each RPM in place, so it puts
// the repo into force-overwrite mode.
func resignPackages(cmd *cobra.Command, r *repo.Repo) (func(), error) {
	signer, cleanupSigner, err := packageSigner()
	if err != nil {
		return nil, err
	}
	// Re-signing replaces the previous key's signature rather than adding to it.
	signer.Replace = true
	r.SetForce(true)

	cleanups := []func(){cleanupSigner}
	cleanupAll := func() {
		for _, c := range cleanups {
			c()
		}
	}
	out := stdout(cmd)
	targets := r.ResignTargets()
	var toDownload int64
	for _, p := range targets {
		toDownload += p.SizePackage
	}
	prog.Begin("download", len(targets), toDownload)
	defer prog.End()

	for _, p := range targets {
		loc := p.Location
		tmp, err := r.GetToFile(ctx(cmd), loc)
		if err != nil {
			cleanupAll()
			return nil, fmt.Errorf("download %s: %w", loc, err)
		}
		signed, cleanup, err := signer.SignFile(tmp)
		os.Remove(tmp) // the downloaded copy is no longer needed once signed
		if err != nil {
			cleanupAll()
			return nil, fmt.Errorf("sign %s: %w", loc, err)
		}
		cleanups = append(cleanups, cleanup)
		if _, err := r.ReplaceFromFile(signed, loc); err != nil {
			cleanupAll()
			return nil, fmt.Errorf("re-index %s: %w", loc, err)
		}
		fmt.Fprintf(out, "re-signed %s\n", loc)
	}
	return cleanupAll, nil
}

// deletedBytes sums the size of the RPM blobs a plan will delete, taking each
// size from the metadata as it was published. Stray files the metadata never
// named (--remove-unreferenced-rpms) have no recorded size and are not counted,
// so the figure is a floor.
func deletedBytes(before []*repodata.Package, plan *repo.Plan) int64 {
	if plan == nil {
		return 0
	}
	size := make(map[string]int64, len(before))
	for _, p := range before {
		size[p.Location] = p.SizePackage
	}
	var total int64
	for _, href := range plan.DeletedRPMs {
		total += size[href]
	}
	return total
}

// rebuildReport carries the numbers shown before a rebuild is confirmed.
type rebuildReport struct {
	location       string
	beforeCount    int
	afterCount     int
	beforePkgBytes int64
	afterPkgBytes  int64
	totalBytes     int64
	totalFiles     int
	sizeListed     bool
	relocated      int
	resignCount    int
	resignBytes    int64
	refreshed      *repo.RefreshResult
	freedBytes     int64
	plan           *repo.Plan
	depProblems    []repodata.DependencyProblem
}

// printRebuildReport renders the before/after summary. Only lines with something
// to report are shown.
func printRebuildReport(cmd *cobra.Command, rep rebuildReport) {
	out := stdout(cmd)
	fmt.Fprintf(out, "Rebuild plan for %s\n", rep.location)
	fmt.Fprintf(out, "  packages:            %d -> %d (%+d)\n",
		rep.beforeCount, rep.afterCount, rep.afterCount-rep.beforeCount)
	fmt.Fprintf(out, "  package payload:     %s -> %s\n",
		humanBytes(rep.beforePkgBytes), humanBytes(rep.afterPkgBytes))
	approx := ""
	if !rep.sizeListed {
		approx = " (estimated from metadata; backend cannot list)"
	}
	fmt.Fprintf(out, "  repository on disk:  %s across %d object(s)%s\n",
		humanBytes(rep.totalBytes), rep.totalFiles, approx)
	// Show the estimated new total when RPMs are actually being deleted. This
	// counts the blobs the plan removes, not the drop in package payload: a
	// duplicate metadata record for a file another record still names shrinks
	// the payload without freeing a byte.
	if rep.freedBytes > 0 {
		fmt.Fprintf(out, "  after rebuild (est.): %s (frees ~%s)\n",
			humanBytes(rep.totalBytes-rep.freedBytes), humanBytes(rep.freedBytes))
	}

	if f := rep.refreshed; f != nil {
		fmt.Fprintf(out, "  re-read from RPMs:   %d package(s)", f.Read)
		if f.Changed > 0 {
			fmt.Fprintf(out, ", %d corrected (the metadata did not match the file)", f.Changed)
		}
		if f.Duplicates > 0 {
			fmt.Fprintf(out, ", %d duplicate record(s) dropped", f.Duplicates)
		}
		fmt.Fprintln(out)
	}
	if rep.relocated > 0 {
		fmt.Fprintf(out, "  relocate:            %d package(s) moved server-side\n", rep.relocated)
	}
	if rep.resignCount > 0 {
		fmt.Fprintf(out, "  re-sign:             %d package(s), ~%s to download\n",
			rep.resignCount, humanBytes(rep.resignBytes))
	}
	if p := rep.plan; p != nil {
		if len(p.Uploads) > 0 {
			fmt.Fprintf(out, "  upload:              %d file(s) (%s)\n", len(p.Uploads), humanBytes(p.BytesToUpload))
		}
		if len(p.DeletedRPMs) > 0 {
			fmt.Fprintf(out, "  gc RPMs:             %d no-longer-referenced file(s)\n", len(p.DeletedRPMs))
		}
		if len(p.UnreferencedRPMs) > 0 {
			fmt.Fprintf(out, "  remove unreferenced: %d stray .rpm file(s)\n", len(p.UnreferencedRPMs))
		}
		if len(p.StaleMetadata) > 0 {
			fmt.Fprintf(out, "  remove stale meta:   %d repodata file(s)\n", len(p.StaleMetadata))
		}
	}
	if len(rep.depProblems) > 0 {
		fmt.Fprintf(out, "  broken deps:         %d unmet intra-repo dependenc(ies) after rebuild:\n", len(rep.depProblems))
		for _, dp := range rep.depProblems {
			fmt.Fprintf(out, "                       %s\n", dp)
		}
	}
}

// promptYesNo asks question on stdout and reads a line from the command's input,
// returning true only for an explicit yes. EOF or a blank line means no.
func promptYesNo(cmd *cobra.Command, question string) bool {
	fmt.Fprintf(stdout(cmd), "%s [y/N] ", question)
	reader := bufio.NewReader(cmd.InOrStdin())
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return false // EOF with no input: treat as "no"
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// humanBytes formats a byte count with a binary (KiB/MiB/...) suffix. It is the
// same rendering the progress display uses, so a size reported before a
// transfer and the one reported during it read alike.
func humanBytes(n int64) string { return progress.Bytes(n) }
