package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/repo"
	"github.com/danudey/createrepo-go/pkg/repoconfig"
	"github.com/danudey/createrepo-go/pkg/sign"
)

// openRepo opens the backend for location, loads any persisted repository
// config, applies the recorded signing settings as defaults for flags the user
// did not set explicitly, and opens the repository. It returns the repo along
// with the previously-persisted config (nil if none) so a later Save can
// preserve fields (name/baseurl) it does not manage.
func openRepo(cmd *cobra.Command, location string, create bool) (*repo.Repo, *repoconfig.Config, error) {
	be, err := backend.Open(ctx(cmd), location)
	if err != nil {
		return nil, nil, err
	}
	return openRepoBackend(cmd, be, create)
}

// openRepoBackend is openRepo for a backend the caller has already opened (and
// whose config it may already have inspected). It takes ownership of be: on
// success the returned Repo closes it, and on failure it is closed here.
func openRepoBackend(cmd *cobra.Command, be backend.Backend, create bool) (*repo.Repo, *repoconfig.Config, error) {
	prev, err := repoconfig.Load(ctx(cmd), be)
	if err != nil {
		closeBackend(be)
		return nil, nil, err
	}
	if err := applyConfigDefaults(cmd, prev); err != nil {
		closeBackend(be)
		return nil, nil, err
	}
	if err := validateSigningFlags(); err != nil {
		closeBackend(be)
		return nil, nil, err
	}

	opts, err := repoOptions(create)
	if err != nil {
		closeBackend(be)
		return nil, nil, err
	}
	r, err := repo.OpenWith(ctx(cmd), be, opts)
	if err != nil {
		return nil, nil, err
	}
	return r, prev, nil
}

// applyConfigDefaults fills in global flags from the persisted config for any
// flag the user did not set on the command line. This makes the recorded
// settings act as defaults: a repository created for a given --target keeps
// using that profile's defaults, and one signed once keeps being signed with the
// same key, without re-specifying the flags.
//
// Precedence, high to low: explicit CLI flags; an explicit CLI --target's
// profile; the recorded --target's profile; the individually recorded settings.
func applyConfigDefaults(cmd *cobra.Command, cfg *repoconfig.Config) error {
	if cfg == nil {
		return nil
	}
	fl := cmd.Flags()

	// Restore the recorded --target (unless the user gave one) and re-resolve
	// its profile so its compression/signature-format defaults apply. preRunE
	// already resolved a CLI-supplied target; this covers the config-supplied
	// one, which is not known until the config is loaded.
	if !fl.Changed("target") && cfg.Target != "" {
		gf.target = cfg.Target
		if err := applyProfile(cmd, nil); err != nil {
			return fmt.Errorf("recorded --target %q in %s: %w", cfg.Target, repoconfig.Path, err)
		}
	}

	// Land uploads in the same subdirectory as before unless the user gave an
	// explicit --location-prefix.
	if !fl.Changed("location-prefix") && cfg.LocationPrefix != "" {
		gf.locationPrefix = cfg.LocationPrefix
	}

	if !fl.Changed("sign-packages") && cfg.SignPackages {
		gf.signPackages = true
	}
	if !fl.Changed("sign-metadata") && cfg.SignMetadata {
		gf.signMetadata = true
	}
	// The recorded signature format is the last effective one (it captures any
	// explicit override made alongside the target). Apply it unless the user
	// gave an explicit --signature-format or --target on the CLI, which win.
	if !fl.Changed("signature-format") && !fl.Changed("target") && cfg.SignatureFormat != "" {
		gf.signatureFormat = cfg.SignatureFormat
	}
	if !fl.Changed("gpg-key") && !fl.Changed("gpg-key-id") && cfg.GPGKeyID != "" {
		gf.gpgKeyID = cfg.GPGKeyID
	}
	return nil
}

// saveRepoConfig writes the repository config after a successful commit,
// recording the effective signing settings (with the key stored as a
// fingerprint) and preserving/overriding the name and base URL. It is a no-op in
// dry-run mode. Failures are reported but do not fail the command: the packages
// and metadata have already been published at this point.
func saveRepoConfig(cmd *cobra.Command, r *repo.Repo, prev *repoconfig.Config, name, baseURL string) {
	cfg := effectiveConfig(cmd, prev, name, baseURL)
	writeRepoConfig(cmd, r.Backend(), &cfg)
}

// effectiveConfig merges the previously-persisted config with the settings this
// invocation ended up using, returning what should be written back.
func effectiveConfig(cmd *cobra.Command, prev *repoconfig.Config, name, baseURL string) repoconfig.Config {
	cfg := repoconfig.Config{}
	if prev != nil {
		cfg = *prev
	}
	if name != "" {
		cfg.Name = name
	}
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}

	cfg.Target = gf.target
	cfg.LocationPrefix = gf.locationPrefix
	cfg.SignPackages = gf.signPackages
	cfg.SignMetadata = gf.signMetadata
	if gf.signPackages || gf.signMetadata {
		cfg.SignatureFormat = gf.signatureFormat
		if fpr, err := sign.Fingerprint(gf.gpgKey, gf.gpgKeyID); err != nil {
			fmt.Fprintf(stderr(cmd), "warning: could not resolve signing key fingerprint: %v\n", err)
		} else if fpr != "" {
			cfg.GPGKeyID = fpr
		}
	}
	return cfg
}

// writeRepoConfig persists cfg to the repository root. It is a no-op in dry-run
// mode, and a failure is reported without failing the command: by this point the
// packages and metadata have already been published.
func writeRepoConfig(cmd *cobra.Command, be backend.Backend, cfg *repoconfig.Config) {
	out := stdout(cmd)
	if gf.dryRun {
		fmt.Fprintf(out, "[dry-run] config: would write %s\n", repoconfig.Path)
		return
	}
	if err := repoconfig.Save(ctx(cmd), be, cfg); err != nil {
		fmt.Fprintf(stderr(cmd), "warning: could not write %s: %v\n", repoconfig.Path, err)
		return
	}
	fmt.Fprintf(out, "config: wrote %s\n", repoconfig.Path)
}

// closeBackend closes a backend that holds resources, ignoring errors. It is
// used on error paths before a Repo (which would otherwise own the close) is
// constructed.
func closeBackend(be backend.Backend) {
	if c, ok := be.(backend.Closer); ok {
		_ = c.Close()
	}
}
