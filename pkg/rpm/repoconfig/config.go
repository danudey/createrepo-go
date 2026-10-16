// Package repoconfig persists a small createrepo-go configuration file
// alongside a repository (at the repo root, next to repodata/). It records the
// repository's identity (name and public base URL), the upload layout
// (location prefix), and the signing choices that were used, so later
// operations can default to the same settings instead of re-specifying them on
// every invocation.
//
// The file is plain JSON and is not part of the rpm-md metadata: dnf/yum ignore
// it, and it is never referenced by repomd.xml.
package repoconfig

import (
	"context"
	"fmt"

	"github.com/danudey/createrepo-go/pkg/configfile"
	"github.com/danudey/createrepo-go/pkg/gpg"

	"github.com/danudey/createrepo-go/pkg/backend"
)

// Path is the repo-root-relative location of the configuration file.
const Path = "createrepo-go.json"

// Config holds the persisted repository defaults.
type Config struct {
	// Name is a human-readable repository name.
	Name string `json:"name,omitempty"`
	// BaseURL is the URL end users fetch the repository from.
	BaseURL string `json:"baseurl,omitempty"`

	// LocationPrefix is the subdirectory under the repo root that uploaded
	// RPMs are placed in (the --location-prefix value). Recording it keeps
	// later uploads landing in the same directory as previous ones without
	// re-specifying the flag.
	LocationPrefix string `json:"location_prefix,omitempty"`

	// Target is the compatibility profile (rhel8/rhel9/rhel10 or an alias)
	// whose defaults were selected, if any. Recording it lets later operations
	// re-apply the profile's current and future defaults (compression,
	// signature format, ...) without re-specifying --target.
	Target string `json:"target,omitempty"`

	// SignPackages records whether RPMs were signed before upload.
	SignPackages bool `json:"sign_packages"`
	// SignMetadata records whether repomd.xml was signed.
	SignMetadata bool `json:"sign_metadata"`
	// SignatureFormat records the package signature layout used ("v4" or
	// "openpgp"); it is meaningful only when SignPackages is set.
	SignatureFormat string `json:"signature_format,omitempty"`
	// GPGKeyID is the primary-key fingerprint of the key used for signing.
	GPGKeyID string `json:"gpg_key_id,omitempty"`

	// CopySource records the repository this one was copied from (the source
	// location given to `copy`). It lets a later `copy --continue` confirm that
	// it is resuming into the same destination rather than overwriting an
	// unrelated repository.
	CopySource string `json:"copy_source,omitempty"`
}

// Load reads the configuration file from the backend. A repository with no
// config file yields (nil, nil) so callers can treat it as "no recorded
// defaults".
//
// The file is read from repositories this tool does not control (copy and
// verify load the source's), so a recorded key id that is not a hexadecimal
// key id or fingerprint is refused rather than handed to gpg or rpmsign.
func Load(ctx context.Context, be backend.Backend) (*Config, error) {
	cfg, err := configfile.Load[Config](ctx, be, Path)
	if err != nil || cfg == nil {
		return cfg, err
	}
	if cfg.GPGKeyID != "" {
		if err := gpg.CheckKeyID(cfg.GPGKeyID); err != nil {
			return nil, fmt.Errorf("%s: gpg_key_id: %w", Path, err)
		}
	}
	return cfg, nil
}

// Save writes cfg to the backend as pretty-printed JSON.
func Save(ctx context.Context, be backend.Backend, cfg *Config) error {
	return configfile.Save(ctx, be, Path, cfg)
}
