package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/danudey/createrepo-go/pkg/repoconfig"
)

func TestApplyConfigDefaults(t *testing.T) {
	// rootCmd() rebinds gf to flag defaults, so construct the command first in
	// each case and drive flag state through ParseFlags (as cobra does at
	// runtime) so Changed() is accurate.
	apply := func(t *testing.T, root *cobra.Command, cfg *repoconfig.Config) {
		t.Helper()
		if err := applyConfigDefaults(root, cfg); err != nil {
			t.Fatalf("applyConfigDefaults: %v", err)
		}
	}

	t.Run("nil config is a no-op", func(t *testing.T) {
		root := rootCmd()
		apply(t, root, nil)
		if gf.signPackages || gf.signMetadata || gf.gpgKeyID != "" {
			t.Errorf("nil config changed flags: %+v", gf)
		}
	})

	t.Run("recorded settings fill unset flags", func(t *testing.T) {
		root := rootCmd()
		cfg := &repoconfig.Config{SignPackages: true, SignMetadata: true, SignatureFormat: "openpgp", GPGKeyID: "FPR"}
		apply(t, root, cfg)
		if !gf.signPackages || !gf.signMetadata {
			t.Errorf("expected sign flags enabled from config, got %+v", gf)
		}
		if gf.signatureFormat != "openpgp" {
			t.Errorf("signatureFormat = %q, want openpgp", gf.signatureFormat)
		}
		if gf.gpgKeyID != "FPR" {
			t.Errorf("gpgKeyID = %q, want FPR", gf.gpgKeyID)
		}
	})

	t.Run("recorded target restores its profile defaults", func(t *testing.T) {
		root := rootCmd()
		apply(t, root, &repoconfig.Config{Target: "rhel9"})
		if gf.target != "rhel9" {
			t.Errorf("target = %q, want rhel9", gf.target)
		}
		if gf.compression != "zstd" {
			t.Errorf("compression = %q, want zstd (rhel9 default)", gf.compression)
		}
		if gf.signatureFormat != "v4" {
			t.Errorf("signatureFormat = %q, want v4 (rhel9 default)", gf.signatureFormat)
		}
	})

	t.Run("recorded signature override survives target restore", func(t *testing.T) {
		// A repo made with --target rhel9 --signature-format openpgp records
		// both; restoring should reproduce the effective openpgp, not the
		// profile's v4.
		root := rootCmd()
		apply(t, root, &repoconfig.Config{Target: "rhel9", SignatureFormat: "openpgp"})
		if gf.compression != "zstd" {
			t.Errorf("compression = %q, want zstd", gf.compression)
		}
		if gf.signatureFormat != "openpgp" {
			t.Errorf("signatureFormat = %q, want openpgp (recorded override)", gf.signatureFormat)
		}
	})

	t.Run("recorded location prefix overrides the default for an unset flag", func(t *testing.T) {
		// The flag defaults to "Packages"; a repo recorded with a different
		// prefix keeps using that prefix when --location-prefix is not given.
		root := rootCmd()
		apply(t, root, &repoconfig.Config{LocationPrefix: "pool"})
		if gf.locationPrefix != "pool" {
			t.Errorf("locationPrefix = %q, want pool", gf.locationPrefix)
		}
	})

	t.Run("explicit location prefix wins over config", func(t *testing.T) {
		root := rootCmd()
		if err := root.ParseFlags([]string{"--location-prefix", "mine"}); err != nil {
			t.Fatal(err)
		}
		apply(t, root, &repoconfig.Config{LocationPrefix: "Packages"})
		if gf.locationPrefix != "mine" {
			t.Errorf("explicit locationPrefix overridden by config: %q", gf.locationPrefix)
		}
	})

	t.Run("explicit key flag wins over config", func(t *testing.T) {
		root := rootCmd()
		if err := root.ParseFlags([]string{"--gpg-key-id", "MINE", "--sign-packages"}); err != nil {
			t.Fatal(err)
		}
		apply(t, root, &repoconfig.Config{GPGKeyID: "OTHER", SignPackages: true})
		if gf.gpgKeyID != "MINE" {
			t.Errorf("explicit gpgKeyID overridden by config: %q", gf.gpgKeyID)
		}
	})

	t.Run("CLI target wins over recorded target and format", func(t *testing.T) {
		root := rootCmd()
		if err := root.ParseFlags([]string{"--target", "rhel10"}); err != nil {
			t.Fatal(err)
		}
		// Stand in for the profile-resolved format that preRunE would set.
		gf.signatureFormat = "openpgp"
		apply(t, root, &repoconfig.Config{Target: "rhel9", SignatureFormat: "v4", SignPackages: true})
		if gf.target != "rhel10" {
			t.Errorf("CLI target overridden by config: %q", gf.target)
		}
		if gf.signatureFormat != "openpgp" {
			t.Errorf("config format overrode --target: %q", gf.signatureFormat)
		}
	})

	gf = globalFlags{}
}

// TestCreateWritesConfig exercises create end-to-end against a local backend:
// the repo config file must be written with the supplied metadata and the
// (unsigned) signing state.
func TestCreateWritesConfig(t *testing.T) {
	gf = globalFlags{}
	defer func() { gf = globalFlags{} }()

	dir := t.TempDir()
	root := rootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"create", dir, "--repo-name", "My Repo", "--repo-url", "https://example.com/repo"})
	if err := root.Execute(); err != nil {
		t.Fatalf("create: %v\n%s", err, buf.String())
	}

	data, err := os.ReadFile(filepath.Join(dir, repoconfig.Path))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg repoconfig.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.Name != "My Repo" {
		t.Errorf("Name = %q, want %q", cfg.Name, "My Repo")
	}
	if cfg.BaseURL != "https://example.com/repo" {
		t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, "https://example.com/repo")
	}
	if cfg.SignPackages || cfg.SignMetadata {
		t.Errorf("unsigned repo recorded as signed: %+v", cfg)
	}
	if cfg.GPGKeyID != "" {
		t.Errorf("unexpected key recorded: %q", cfg.GPGKeyID)
	}
}

// TestCreateConfigDryRun ensures dry-run does not write the config file.
func TestCreateConfigDryRun(t *testing.T) {
	gf = globalFlags{}
	defer func() { gf = globalFlags{} }()

	dir := t.TempDir()
	root := rootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"create", dir, "--repo-name", "X", "--dry-run"})
	if err := root.Execute(); err != nil {
		t.Fatalf("create --dry-run: %v\n%s", err, buf.String())
	}
	if _, err := os.Stat(filepath.Join(dir, repoconfig.Path)); !os.IsNotExist(err) {
		t.Errorf("dry-run wrote a config file (err=%v)", err)
	}
}
