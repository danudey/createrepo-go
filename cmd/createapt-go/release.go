package main

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/danudey/createrepo-go/internal/cli"
	"github.com/danudey/createrepo-go/pkg/apt/repo"
	"github.com/danudey/createrepo-go/pkg/apt/repoconfig"
	"github.com/danudey/createrepo-go/pkg/apt/sign"
	"github.com/danudey/createrepo-go/pkg/backend"
)

// verifyReleaseSignature checks a suite's Release signature in every form the
// suite publishes, and reports whether it is signed at all. whose names the
// repository in messages ("the source's", "the").
//
// apt reads InRelease when it can and falls back to Release with Release.gpg,
// so both forms are checked when both are present, and InRelease's signed
// text must be the Release file: otherwise clients that fall back would be
// reading a different, possibly unsigned, document from the one that was
// verified.
func verifyReleaseSignature(cmd *cobra.Command, r *repo.Repo, keyrings []string, whose string) (bool, error) {
	ctx := cli.Ctx(cmd)
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	inRelease, detached, err := r.ReleaseSignatures(ctx)
	if err != nil {
		return false, fmt.Errorf("reading %s Release signature: %w", whose, err)
	}
	signed := inRelease != nil || detached != nil

	if gf.skipVerify {
		if signed {
			fmt.Fprintf(errOut, "warning: --skip-verify: %s Release signature is not being checked\n", whose)
		}
		return signed, nil
	}
	if len(keyrings) == 0 {
		if gf.verifySigs {
			return signed, fmt.Errorf("--verify-sigs needs a public key: pass --keyring, or use a repository whose %s records its signing key", repoconfig.Path)
		}
		if signed {
			fmt.Fprintf(errOut, "warning: %s Release is signed but no public key is available to check it; pass --keyring to verify it\n", whose)
		}
		return signed, nil
	}
	if !signed {
		if gf.verifySigs {
			return false, fmt.Errorf("--verify-sigs: suite %s carries no Release signature", r.Suite())
		}
		fmt.Fprintf(errOut, "warning: %s Release is not signed (no InRelease or Release.gpg)\n", whose)
		return false, nil
	}

	raw, err := r.RawRelease(ctx)
	if errors.Is(err, backend.ErrNotExist) {
		raw = nil
	} else if err != nil {
		return true, fmt.Errorf("reading %s Release: %w", whose, err)
	}

	if inRelease != nil {
		body, fpr, err := sign.VerifyClearsigned(inRelease, keyrings)
		if err != nil {
			return true, fmt.Errorf("%s InRelease signature does not verify: %w", whose, err)
		}
		fmt.Fprintf(out, "verified: InRelease signed by %s\n", fpr)
		// A clearsigned document does not sign the line ending before its
		// signature, so the comparison ignores trailing newlines.
		if raw != nil && !bytes.Equal(bytes.TrimRight(body, "\r\n"), bytes.TrimRight(raw, "\r\n")) {
			return true, fmt.Errorf("%s InRelease and Release differ: clients that read Release would see a different document from the one InRelease signs", whose)
		}
	}
	if detached != nil {
		if raw == nil {
			return true, fmt.Errorf("%s Release.gpg is present but Release is not", whose)
		}
		fpr, err := sign.VerifyDetached(raw, detached, keyrings)
		if err != nil {
			return true, fmt.Errorf("%s Release.gpg signature does not verify: %w", whose, err)
		}
		fmt.Fprintf(out, "verified: Release signed by %s\n", fpr)
	}
	return true, nil
}
