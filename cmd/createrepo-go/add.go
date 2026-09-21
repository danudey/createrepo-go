package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danudey/createrepo-go/pkg/sign"
)

func addCmd() *cobra.Command {
	var prune bool
	cmd := &cobra.Command{
		Use:   "add <repo-url> <rpm-or-dir>...",
		Short: "Add (or update) RPMs in a repository",
		Long: `Upload one or more RPMs and add them to the live repository metadata,
creating the repository if it does not yet exist.

Each positional argument may be a .rpm file or a directory. Directories are
scanned recursively and every .rpm file found within them (including in
subdirectories) is added.

A package whose name+version+release+arch already exists is replaced. With
--prune-older, older versions of the same name+arch are also removed. Whenever
an RPM stops being referenced by the metadata — because it was replaced, pruned,
or moved to a new location — its old file is garbage-collected on publish. A
relocated package (identical content at a new location, e.g. under a new
--location-prefix) is moved server-side rather than re-uploaded when the backend
supports it.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			location := args[0]
			rpms, err := expandRPMArgs(args[1:])
			if err != nil {
				return err
			}

			// Open the repo first so persisted signing defaults are applied
			// before packages are verified/signed below.
			r, prev, err := openRepo(cmd, location, true)
			if err != nil {
				return err
			}
			defer r.Close()
			r.SetPruneOlder(prune)

			// Optional verification of incoming signatures.
			if gf.verifySigs {
				v, err := sign.NewVerifier(gf.keyrings...)
				if err != nil {
					return err
				}
				defer v.Close()
				for _, p := range rpms {
					if err := v.VerifyFile(p); err != nil {
						return err
					}
				}
			}

			// Optional signing of packages before upload.
			if gf.signPackages {
				signer, cleanupAll, err := packageSigner()
				if err != nil {
					return err
				}
				defer cleanupAll()
				signed := make([]string, len(rpms))
				for i, p := range rpms {
					out, cleanup, err := signer.SignFile(p)
					if err != nil {
						return err
					}
					defer cleanup()
					signed[i] = out
				}
				rpms = signed
			}

			for _, p := range rpms {
				pkg, err := r.AddRPM(p)
				if err != nil {
					return fmt.Errorf("add %s: %w", p, err)
				}
				fmt.Fprintf(stdout(cmd), "staged %s -> %s\n", pkg.NEVRA(), pkg.Location)
			}
			kept, broken := r.PruneWarnings()
			printPruneWarnings(cmd, kept, broken)

			plan, err := r.Commit(ctx(cmd))
			if err != nil {
				return err
			}
			printPlan(cmd, plan, gf.dryRun)
			saveRepoConfig(cmd, r, prev, "", "")
			return nil
		},
	}
	cmd.Flags().BoolVar(&prune, "prune-older", false, "remove older versions of the same name+arch")
	cmd.Flags().BoolVar(&gf.pruneBreakDeps, "prune-break-deps", false, "when pruning, drop a version even if another package depends on it (default: keep it and warn)")
	deprecateDeleteRemoved(cmd)
	return cmd
}

// deprecateDeleteRemoved registers the old --delete-removed flag as a hidden
// no-op so existing invocations keep working: unreferenced RPMs are now always
// garbage-collected on publish, so the flag no longer has any effect.
func deprecateDeleteRemoved(cmd *cobra.Command) {
	var ignored bool
	cmd.Flags().BoolVar(&ignored, "delete-removed", false, "deprecated: no-op; unreferenced RPMs are always garbage-collected")
	_ = cmd.Flags().MarkDeprecated("delete-removed", "unreferenced RPMs are now garbage-collected automatically")
}

// expandRPMArgs turns the positional arguments — each a .rpm file or a
// directory — into a deduplicated, sorted list of file paths to add.
// Directories are walked recursively and only files with a .rpm extension are
// collected; an explicitly named file is kept as given (so a caller can add a
// package whose name does not end in .rpm). It returns an error if a path does
// not exist or if no files were found.
func expandRPMArgs(paths []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		key := p
		if abs, err := filepath.Abs(p); err == nil {
			key = abs
		}
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, p)
	}

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if !info.IsDir() {
			add(p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".rpm") {
				add(path)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scanning %s: %w", p, err)
		}
	}

	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("no .rpm files found in the given path(s)")
	}
	return out, nil
}

// packageSigner builds an RPM package signer from the global flags and returns
// it with a cleanup function for any temporary GnuPG home.
func packageSigner() (*sign.PackageSigner, func(), error) {
	format, err := signatureFormat()
	if err != nil {
		return nil, func() {}, err
	}
	switch {
	case gf.gpgKey != "":
		s, err := sign.NewPackageSignerKeyFile(gf.gpgKey, gf.gpgPass)
		if err != nil {
			return nil, func() {}, err
		}
		s.Format = format
		return s, s.Close, nil
	case gf.gpgKeyID != "":
		s := sign.NewPackageSignerKeyID(gf.gpgKeyID, gf.gpgPass)
		s.Format = format
		return s, func() {}, nil
	default:
		return nil, func() {}, fmt.Errorf("--sign-packages requires --gpg-key or --gpg-key-id")
	}
}

// signatureFormat parses and validates the --signature-format flag.
// The accepted --signature-format values.
const (
	sigFormatV4      = "v4"
	sigFormatOpenPGP = "openpgp"
)

func signatureFormat() (sign.SignatureFormat, error) {
	switch gf.signatureFormat {
	case "", sigFormatV4:
		return sign.SigV4, nil
	case sigFormatOpenPGP:
		return sign.SigOpenPGP, nil
	default:
		return "", fmt.Errorf("invalid --signature-format %q (use v4 or openpgp)", gf.signatureFormat)
	}
}
