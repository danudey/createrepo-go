//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/danudey/createrepo-go/pkg/sign"
)

// scenario is one backend-agnostic behavior check, run against every writable
// backend.
type scenario struct {
	name       string
	needsNewer bool // requires the bumped hello-2.10-4 build (rpmbuild)
	needsDep   bool // requires the deplib/depapp dependency fixtures (rpmbuild)
	run        func(c *tctx, r rpmSet)
}

func base(p string) string { return filepath.Base(p) }

// progressBytesRE picks the transferred and expected byte figures out of the
// summary line --progress leaves behind ("upload: 2/2 complete, 13.9 KiB/13.9
// KiB in 0s ...").
var progressBytesRE = regexp.MustCompile(`complete, ([\d.]+ \w+)/([\d.]+ \w+) in`)

// loc is the repo-relative location an RPM lands at with the default
// --location-prefix ("Packages"), i.e. where a bare `add` uploads it.
func loc(p string) string { return "Packages/" + base(p) }

// copyInto copies the file src into directory dir (created if needed) and
// returns the destination path.
func copyInto(t *testing.T, src, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	dst := filepath.Join(dir, base(src))
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
	return dst
}

func primaryLoc(locs []string) string {
	for _, l := range locs {
		if strings.Contains(l, "primary") {
			return l
		}
	}
	return ""
}

var scenarios = []scenario{
	{name: "create_empty", run: func(c *tctx, r rpmSet) {
		c.create()
		c.mustExist("repodata/repomd.xml")
		c.wantCount(0)
		c.verifyOK()
	}},

	{name: "add_creates_repo", run: func(c *tctx, r rpmSet) {
		c.add(r.hello)
		c.mustExist("repodata/repomd.xml")
		c.mustExist(loc(r.hello))
		c.wantCount(1)
		c.wantListContains("hello")
		c.verifyOK()
	}},

	{name: "add_multiple_packages", run: func(c *tctx, r rpmSet) {
		c.add(r.hello, r.libfoo)
		c.mustExist(loc(r.hello))
		c.mustExist(loc(r.libfoo))
		c.wantCount(2)
		c.wantListContains("hello", "libfoo")
		c.verifyOK()
	}},

	{name: "add_with_progress", run: func(c *tctx, r rpmSet) {
		// --progress wraps the reader handed to every backend, which the object
		// stores need to keep seekable; running this against each backend is
		// what proves the upload still works with the display on.
		res := c.addWith([]string{"--progress"}, r.hello, r.libfoo)
		if !strings.Contains(res.stderr, "upload: 2/2 complete") {
			c.t.Errorf("--progress did not report the upload on stderr:\n%s", res.combined())
		}
		// The bytes reported must be the bytes the packages hold. On the object
		// stores the body is read twice — hashed, then sent — so this is what
		// proves the second pass is what gets counted rather than both.
		if m := progressBytesRE.FindStringSubmatch(res.stderr); m == nil {
			c.t.Errorf("no progress summary to read a byte count from:\n%s", res.stderr)
		} else if m[1] != m[2] {
			c.t.Errorf("progress reported %s of %s uploaded, want the two to agree:\n%s", m[1], m[2], res.stderr)
		}
		// The display belongs on stderr only: a caller reading the command's
		// output must see exactly what it would have seen without --progress.
		if strings.Contains(res.stdout, "upload: 2/2 complete") {
			c.t.Errorf("progress reporting leaked into stdout:\n%s", res.stdout)
		}
		c.wantCount(2)
		c.mustExist(loc(r.hello))
		c.mustExist(loc(r.libfoo))
		c.verifyOK()
		c.checkOK("fetch")
	}},

	{name: "add_directory_recursive", run: func(c *tctx, r rpmSet) {
		// Lay the RPMs out in a nested tree and add the top directory; both
		// packages should be discovered across the subdirectories.
		src := c.t.TempDir()
		copyInto(c.t, r.hello, filepath.Join(src, "noarch"))
		copyInto(c.t, r.libfoo, filepath.Join(src, "arch", "x86_64"))
		c.add(src)
		c.wantCount(2)
		c.wantListContains("hello", "libfoo")
		c.verifyOK()
	}},

	{name: "incremental_add_gc_old_metadata", run: func(c *tctx, r rpmSet) {
		c.add(r.hello)
		oldPrimary := primaryLoc(c.repomdLocations())
		if oldPrimary == "" {
			c.t.Fatal("no primary metadata after first add")
		}
		// Second, separate invocation adds libfoo to the live repo.
		res := c.add(r.libfoo)
		if !strings.Contains(res.combined(), "upload") {
			c.t.Errorf("incremental add did not report an upload:\n%s", res.combined())
		}
		c.wantCount(2)
		// New primary differs and the old one is garbage-collected.
		newPrimary := primaryLoc(c.repomdLocations())
		if newPrimary == oldPrimary {
			c.t.Errorf("primary metadata href did not change after update")
		}
		c.mustMissing(oldPrimary)
		c.mustExist(newPrimary)
		c.verifyOK()
	}},

	{name: "idempotent_readd", run: func(c *tctx, r rpmSet) {
		c.add(r.hello)
		res := c.add(r.hello)
		if !strings.Contains(res.combined(), "skip") {
			c.t.Errorf("re-adding identical RPM did not skip:\n%s", res.combined())
		}
		c.wantCount(1)
		c.verifyOK()
	}},

	{name: "force_reupload", run: func(c *tctx, r rpmSet) {
		c.add(r.hello)
		res := c.addWith([]string{"--force"}, r.hello)
		if !strings.Contains(res.combined(), "upload") ||
			!strings.Contains(res.combined(), "force") {
			c.t.Errorf("--force re-add did not report a forced upload:\n%s", res.combined())
		}
		c.wantCount(1)
		c.verifyOK()
	}},

	{name: "coexisting_versions", needsNewer: true, run: func(c *tctx, r rpmSet) {
		c.add(r.hello)
		c.add(r.helloNewer)
		c.wantCount(2) // both EVRs of hello coexist without --prune-older
		c.wantListContains("2.10-3", "2.10-4")
		c.mustExist(loc(r.hello))
		c.mustExist(loc(r.helloNewer))
		c.verifyOK()
	}},

	{name: "prune_older_gc_blob", needsNewer: true, run: func(c *tctx, r rpmSet) {
		c.add(r.hello)
		c.addWith([]string{"--prune-older"}, r.helloNewer)
		c.wantCount(1)
		c.wantListContains("2.10-4")
		c.wantListExcludes("2.10-3")
		c.mustMissing(loc(r.hello)) // pruned blob is garbage-collected automatically
		c.mustExist(loc(r.helloNewer))
		c.verifyOK()
	}},

	{name: "remove_gc_blob", run: func(c *tctx, r rpmSet) {
		c.add(r.hello, r.libfoo)
		c.cli("remove", c.repo, "hello")
		c.wantCount(1)
		c.wantListExcludes("hello")
		c.mustMissing(loc(r.hello)) // no-longer-referenced blob is garbage-collected
		c.mustExist(loc(r.libfoo))
		c.verifyOK()
	}},

	{name: "relocate_moves_blob", run: func(c *tctx, r rpmSet) {
		c.add(r.hello) // lands under the default prefix (Packages/)
		c.mustExist(loc(r.hello))
		// Re-add the identical RPM under a different location prefix: it should
		// move to the new path and the old copy should be gone, without
		// re-uploading.
		res := c.addWith([]string{"--location-prefix", "pool"}, r.hello)
		if !strings.Contains(res.combined(), "move") {
			c.t.Errorf("relocation did not report a move:\n%s", res.combined())
		}
		if !strings.Contains(res.combined(), "0 upload(s)") {
			c.t.Errorf("relocation should not re-upload:\n%s", res.combined())
		}
		c.wantCount(1)
		c.mustMissing(loc(r.hello))                       // old location gone
		c.mustExist(filepath.Join("pool", base(r.hello))) // new location present
		c.verifyOK()
	}},

	{name: "remove_by_arch", run: func(c *tctx, r rpmSet) {
		c.add(r.hello, r.libfoo)
		c.cli("remove", "--arch", "x86_64", c.repo, "libfoo")
		c.wantCount(1)
		c.wantListContains("hello")
		c.wantListExcludes("libfoo")
		c.verifyOK()
	}},

	{name: "remove_nonexistent_errors", run: func(c *tctx, r rpmSet) {
		c.add(r.hello)
		c.cliErr("remove", c.repo, "no-such-package")
		c.wantCount(1) // unchanged
	}},

	{name: "location_prefix", run: func(c *tctx, r rpmSet) {
		// An explicit prefix overrides the default (Packages/).
		c.addWith([]string{"--location-prefix", "custom"}, r.hello)
		c.mustExist("custom/" + base(r.hello))
		c.mustMissing(loc(r.hello))
		c.wantCount(1)
		c.wantListContains("custom/")
		c.verifyOK()
	}},

	{name: "compression_zstd", run: func(c *tctx, r rpmSet) {
		c.addWith([]string{"--compression", "zstd"}, r.hello)
		var hasZst bool
		for _, l := range c.repomdLocations() {
			if strings.HasSuffix(l, ".zst") {
				hasZst = true
			}
		}
		if !hasZst {
			c.t.Errorf("zstd metadata not found in repomd locations: %v", c.repomdLocations())
		}
		c.wantCount(1)
		c.verifyOK()
	}},

	{name: "dry_run_writes_nothing", run: func(c *tctx, r rpmSet) {
		res := c.addWith([]string{"--dry-run"}, r.hello)
		if !strings.Contains(res.combined(), "[dry-run]") {
			c.t.Errorf("dry-run output missing [dry-run] marker:\n%s", res.combined())
		}
		c.mustMissing("repodata/repomd.xml")
		c.mustMissing(loc(r.hello))
	}},

	{name: "sign_metadata", run: func(c *tctx, r rpmSet) {
		k := setupGPGKey(c.t)
		c.extraEnv = k.env()
		c.addWith([]string{"--sign-metadata", "--gpg-key-id", k.keyID}, r.hello)
		c.mustExist("repodata/repomd.xml.asc")
		sig := c.h.ReadAll(c.t, c.repo, "repodata/repomd.xml.asc")
		data := c.h.ReadAll(c.t, c.repo, "repodata/repomd.xml")
		if err := gpgVerifyDetached(c.t, k, sig, data); err != nil {
			c.t.Errorf("repomd.xml.asc did not verify: %v", err)
		}
		c.verifyOK()
	}},

	{name: "sign_packages", run: func(c *tctx, r rpmSet) {
		k := setupGPGKey(c.t)
		c.extraEnv = k.env()
		c.addWith([]string{"--sign-packages", "--gpg-key-id", k.keyID}, r.hello)
		uploaded := c.h.ReadAll(c.t, c.repo, loc(r.hello))
		original, err := os.ReadFile(r.hello)
		must(c.t, err)
		if bytes.Equal(uploaded, original) {
			c.t.Errorf("uploaded RPM is byte-identical to the unsigned original")
		}
		// The uploaded RPM must verify against the signing key.
		tmp := filepath.Join(c.t.TempDir(), base(r.hello))
		must(c.t, os.WriteFile(tmp, uploaded, 0o644))
		v, err := sign.NewVerifier(k.pubFile)
		must(c.t, err)
		defer v.Close()
		if err := v.VerifyFile(tmp); err != nil {
			c.t.Errorf("uploaded RPM should verify as signed: %v", err)
		}
		c.verifyOK()
	}},

	{name: "verify_sigs_gate", run: func(c *tctx, r rpmSet) {
		k := setupGPGKey(c.t)
		c.extraEnv = k.env()
		// Unsigned RPM is rejected when --verify-sigs is on.
		c.cliErr("add", "--verify-sigs", "--keyring", k.pubFile, c.repo, r.hello)
		c.mustMissing("repodata/repomd.xml")
		// A pre-signed RPM passes the gate and is added.
		signer := sign.NewPackageSignerKeyID(k.keyID, "")
		signed, cleanup, err := signer.SignFile(r.hello)
		must(c.t, err)
		defer cleanup()
		c.cli("add", "--verify-sigs", "--keyring", k.pubFile, c.repo, signed)
		c.wantCount(1)
		c.verifyOK()
	}},

	{name: "repeated_update_stress", run: func(c *tctx, r rpmSet) {
		prevPrimary := ""
		step := func(wantCount int) {
			c.t.Helper()
			c.wantCount(wantCount)
			c.verifyOK()
			cur := primaryLoc(c.repomdLocations())
			if prevPrimary != "" && cur != prevPrimary {
				c.mustMissing(prevPrimary) // old metadata GC'd each publish
			}
			prevPrimary = cur
		}
		c.add(r.hello)
		step(1)
		c.add(r.libfoo)
		step(2)
		c.cli("remove", c.repo, "hello")
		step(1)
		c.add(r.hello)
		step(2)
		if r.haveNewer() {
			c.addWith([]string{"--prune-older"}, r.helloNewer)
			step(2) // hello replaced by helloNewer, libfoo still present
		}
	}},

	{name: "minimal_transfer_skips_present", run: func(c *tctx, r rpmSet) {
		c.add(r.libfoo)
		res := c.add(r.libfoo) // re-add to the existing repo
		if !strings.Contains(res.combined(), "skip") {
			c.t.Errorf("re-add of present RPM was not skipped:\n%s", res.combined())
		}
		if !strings.Contains(res.combined(), "present") {
			c.t.Errorf("skip reason did not mention the file being present:\n%s", res.combined())
		}
		if !strings.Contains(res.combined(), "0 upload(s)") {
			c.t.Errorf("expected no uploads on re-add:\n%s", res.combined())
		}
		c.wantCount(1)
	}},

	{name: "verify_detects_missing_blob", run: func(c *tctx, r rpmSet) {
		c.add(r.hello)
		c.h.Remove(c.t, c.repo, loc(r.hello))
		res := c.verifyBad()
		if !strings.Contains(res.combined(), "missing") {
			c.t.Errorf("verify did not report the missing RPM:\n%s", res.combined())
		}
		// Re-adding restores the blob and a clean verify.
		c.addWith([]string{"--force"}, r.hello)
		c.verifyOK()
	}},

	{name: "verify_detects_corruption", run: func(c *tctx, r rpmSet) {
		c.add(r.hello)
		c.h.WriteRaw(c.t, c.repo, loc(r.hello), []byte("not a real rpm"))
		res := c.verifyBad()
		out := res.combined()
		if !strings.Contains(out, "size") && !strings.Contains(out, "checksum") {
			c.t.Errorf("verify did not report size/checksum mismatch:\n%s", out)
		}
	}},

	{name: "check_levels_pass", run: func(c *tctx, r rpmSet) {
		c.add(r.hello, r.libfoo)
		// Every layer of validation should pass on a freshly published repo.
		c.checkOK("metadata")
		c.checkOK("head")
		c.checkOK("fetch")
	}},

	{name: "rebuild_prune_older", needsNewer: true, run: func(c *tctx, r rpmSet) {
		c.add(r.hello)
		c.add(r.helloNewer)
		c.wantCount(2) // both EVRs coexist
		// rebuild --prune-older keeps only the newest of each name+arch.
		c.cli("rebuild", c.repo, "--prune-older", "--yes")
		c.wantCount(1)
		c.wantListContains("2.10-4")
		c.wantListExcludes("2.10-3")
		c.mustMissing(loc(r.hello))    // pruned blob garbage-collected
		c.mustExist(loc(r.helloNewer)) // surviving version present
		c.verifyOK()
	}},

	{name: "prune_older_protects_dependent", needsDep: true, run: func(c *tctx, r rpmSet) {
		// depapp requires deplib = 1.0 specifically; deplib-2.0 is also present.
		c.add(r.depApp, r.depLibOld, r.depLibNew)
		c.wantCount(3)
		c.verifyOK()

		// rebuild --prune-older would normally drop deplib-1.0, but depapp still
		// depends on it, so it is kept and a warning is emitted.
		res := c.cli("rebuild", c.repo, "--prune-older", "--yes")
		if !strings.Contains(res.combined(), "keeping deplib-1.0") {
			c.t.Errorf("rebuild --prune-older did not warn about the protected version:\n%s", res.combined())
		}
		c.wantCount(3)
		c.wantListContains("deplib-1.0", "deplib-2.0")
		c.mustExist(loc(r.depLibOld))
		c.verifyOK()
	}},

	{name: "prune_older_break_deps", needsDep: true, run: func(c *tctx, r rpmSet) {
		c.add(r.depApp, r.depLibOld, r.depLibNew)
		c.wantCount(3)
		// --prune-break-deps drops deplib-1.0 despite depapp needing it, and the
		// resulting broken dependency is reported and later caught by verify.
		res := c.cli("rebuild", c.repo, "--prune-older", "--prune-break-deps", "--yes")
		if !strings.Contains(res.combined(), "pruned deplib-1.0") {
			c.t.Errorf("rebuild --prune-break-deps did not warn about the dropped version:\n%s", res.combined())
		}
		c.wantCount(2)
		c.wantListExcludes("deplib-1.0")
		c.mustMissing(loc(r.depLibOld))
		// The repository now has a broken intra-repo dependency; verify must fail.
		if res := c.verifyBad(); !strings.Contains(res.combined(), "deplib = 1.0") {
			c.t.Errorf("verify did not report the broken dependency:\n%s", res.combined())
		}
	}},

	{name: "rebuild_relocate", run: func(c *tctx, r rpmSet) {
		c.add(r.hello) // lands under the default prefix (Packages/)
		c.mustExist(loc(r.hello))
		// rebuild with a new prefix relocates existing packages server-side.
		res := c.cli("rebuild", c.repo, "--location-prefix", "pool", "--yes")
		if !strings.Contains(res.combined(), "move") {
			c.t.Errorf("rebuild relocation did not report a move:\n%s", res.combined())
		}
		if !strings.Contains(res.combined(), "0 upload(s)") {
			c.t.Errorf("rebuild relocation should not re-upload:\n%s", res.combined())
		}
		c.wantCount(1)
		c.mustMissing(loc(r.hello))                       // old location gone
		c.mustExist(filepath.Join("pool", base(r.hello))) // new location present
		c.verifyOK()
	}},

	{name: "rebuild_remove_unreferenced_rpms", run: func(c *tctx, r rpmSet) {
		c.add(r.hello, r.libfoo)
		// Drop a stray RPM the metadata does not reference.
		stray := filepath.Join("Packages", "stray-9.9-9.noarch.rpm")
		c.h.WriteRaw(c.t, c.repo, stray, []byte("not a real rpm"))
		c.cli("rebuild", c.repo, "--remove-unreferenced-rpms", "--yes")
		c.mustMissing(stray)      // stray file removed
		c.mustExist(loc(r.hello)) // referenced RPMs untouched
		c.mustExist(loc(r.libfoo))
		c.wantCount(2)
		c.verifyOK()
	}},

	{name: "rebuild_remove_stale_metadata", run: func(c *tctx, r rpmSet) {
		c.add(r.hello)
		// Drop a stray repodata file that repomd.xml does not reference.
		stale := filepath.Join("repodata", "leftover-primary.xml.gz")
		c.h.WriteRaw(c.t, c.repo, stale, []byte("junk"))
		c.cli("rebuild", c.repo, "--remove-stale-metadata", "--yes")
		c.mustMissing(stale)               // stale metadata removed
		c.mustExist("repodata/repomd.xml") // live metadata intact
		c.wantCount(1)
		c.verifyOK()
		c.checkOK("metadata")
	}},

	{name: "rebuild_confirm_prompt", run: func(c *tctx, r rpmSet) {
		c.add(r.hello, r.libfoo)
		stray := filepath.Join("Packages", "stray-9.9-9.noarch.rpm")
		c.h.WriteRaw(c.t, c.repo, stray, []byte("not a real rpm"))
		// Answering "no" makes no changes.
		res := c.cliInput("n\n", "rebuild", c.repo, "--remove-unreferenced-rpms")
		if !strings.Contains(res.combined(), "aborted") {
			c.t.Errorf("declining the prompt did not abort:\n%s", res.combined())
		}
		c.mustExist(stray) // nothing removed on abort
		// Answering "yes" proceeds.
		c.cliInput("y\n", "rebuild", c.repo, "--remove-unreferenced-rpms")
		c.mustMissing(stray)
		c.verifyOK()
	}},

	{name: "rebuild_dry_run", run: func(c *tctx, r rpmSet) {
		c.add(r.hello)
		stray := filepath.Join("Packages", "stray-9.9-9.noarch.rpm")
		c.h.WriteRaw(c.t, c.repo, stray, []byte("not a real rpm"))
		res := c.cli("rebuild", c.repo, "--remove-unreferenced-rpms", "--dry-run")
		if !strings.Contains(res.combined(), "[dry-run]") {
			c.t.Errorf("dry-run output missing [dry-run] marker:\n%s", res.combined())
		}
		c.mustExist(stray) // dry-run changes nothing
		c.wantCount(1)
	}},

	{name: "rebuild_resign", run: func(c *tctx, r rpmSet) {
		keyA := setupGPGKey(c.t)
		c.extraEnv = keyA.env()
		c.addWith([]string{"--sign-packages", "--gpg-key-id", keyA.keyID}, r.hello)

		// Re-sign every RPM with a second key.
		keyB := newGPGKey(c.t)
		c.extraEnv = keyB.env()
		c.cli("rebuild", c.repo, "--resign-packages", "--gpg-key-id", keyB.keyID, "--yes")

		// The re-signed RPM must verify against the new key.
		uploaded := c.h.ReadAll(c.t, c.repo, loc(r.hello))
		tmp := filepath.Join(c.t.TempDir(), base(r.hello))
		must(c.t, os.WriteFile(tmp, uploaded, 0o644))
		v, err := sign.NewVerifier(keyB.pubFile)
		must(c.t, err)
		defer v.Close()
		if err := v.VerifyFile(tmp); err != nil {
			c.t.Errorf("re-signed RPM should verify against the new key: %v", err)
		}
		c.wantCount(1)
		c.verifyOK()
	}},

	{name: "check_detects_corruption", run: func(c *tctx, r rpmSet) {
		c.add(r.hello)
		// Publish different-sized content over the indexed RPM so its size and
		// checksum diverge from the metadata.
		c.h.WriteRaw(c.t, c.repo, loc(r.hello), []byte("not a real rpm"))
		res := c.checkBad("head")
		out := res.combined()
		if !strings.Contains(out, "size") && !strings.Contains(out, "checksum") {
			c.t.Errorf("check did not report the size/checksum mismatch:\n%s", out)
		}
		// Re-publishing the real RPM restores a clean check.
		c.addWith([]string{"--force"}, r.hello)
		c.checkOK("head")
	}},

	{name: "verify_checksums_detects_same_size_corruption", run: func(c *tctx, r rpmSet) {
		c.add(r.hello)
		// Replace the RPM with content of exactly the same length, so nothing
		// but a hash over the bytes can notice.
		good := c.h.ReadAll(c.t, c.repo, loc(r.hello))
		bad := append([]byte(nil), good...)
		bad[len(bad)/2] ^= 0xff
		c.h.WriteRaw(c.t, c.repo, loc(r.hello), bad)

		res := c.cliErr("verify", c.repo, "--checksums")
		if !strings.Contains(res.combined(), "content checksum") {
			c.t.Errorf("verify --checksums did not report the content mismatch:\n%s", res.combined())
		}

		// Putting the real bytes back makes it verify from content again.
		c.h.WriteRaw(c.t, c.repo, loc(r.hello), good)
		res = c.cli("verify", c.repo, "--checksums")
		if !strings.Contains(res.stdout, "verified from content") {
			c.t.Errorf("verify --checksums did not report a content-proved checksum:\n%s", res.combined())
		}
	}},

	{name: "takeover_reports_without_changing_anything", run: func(c *tctx, r rpmSet) {
		c.add(r.hello, r.libfoo)
		// Make it look like a repository somebody else published: drop the
		// config file this tool records, which is what takeover starts from.
		c.h.Remove(c.t, c.repo, "createrepo-go.json")
		before := c.repomdLocations()

		res := c.cli("takeover", c.repo)
		if !strings.Contains(res.combined(), "Verdict: 0 blocking") {
			c.t.Errorf("a repository this tool itself published should have no blocking findings:\n%s", res.combined())
		}
		// The analysis is read-only: same metadata files, same packages.
		if got := c.repomdLocations(); strings.Join(got, ",") != strings.Join(before, ",") {
			c.t.Errorf("takeover republished the metadata:\nbefore %v\nafter  %v", before, got)
		}
		if c.h.Exists(c.t, c.repo, "createrepo-go.json") {
			c.t.Error("takeover wrote the config file without --adopt")
		}
		c.wantCount(2)
		c.verifyOK()

		// Adopting records the settings and still publishes nothing.
		c.cli("takeover", c.repo, "--adopt", "--yes", "--repo-name", "Taken Over")
		c.mustExist("createrepo-go.json")
		if got := c.repomdLocations(); strings.Join(got, ",") != strings.Join(before, ",") {
			c.t.Errorf("--adopt republished the metadata:\nbefore %v\nafter  %v", before, got)
		}
		c.verifyOK()
	}},

	{name: "takeover_refuses_to_invalidate_a_signature", run: func(c *tctx, r rpmSet) {
		c.add(r.hello)
		// A detached signature the republish would leave behind, describing a
		// repomd.xml that would no longer exist.
		c.h.WriteRaw(c.t, c.repo, "repodata/repomd.xml.asc",
			[]byte("-----BEGIN PGP SIGNATURE-----\n\nx\n-----END PGP SIGNATURE-----\n"))

		res := c.cliErr("takeover", c.repo)
		if !strings.Contains(res.combined(), "metadata-signature-invalidated") {
			c.t.Errorf("takeover did not report the signature it would invalidate:\n%s", res.combined())
		}
		if !strings.Contains(res.combined(), "not safe to republish") {
			c.t.Errorf("the verdict does not say the takeover is unsafe:\n%s", res.combined())
		}
	}},

	{name: "copy_exact_replica", run: func(c *tctx, r rpmSet) {
		c.add(r.hello, r.libfoo)
		dst := c.h.RepoURL(c.t)
		c.cli("copy", c.repo, dst)

		// Every file the source published is present at the destination, and
		// the RPMs are byte-identical.
		for _, rel := range append([]string{"repodata/repomd.xml"}, c.repomdLocations()...) {
			if !c.h.Exists(c.t, dst, rel) {
				c.t.Errorf("copy did not write %s", rel)
			}
		}
		for _, rpm := range []string{loc(r.hello), loc(r.libfoo)} {
			if !bytes.Equal(c.h.ReadAll(c.t, c.repo, rpm), c.h.ReadAll(c.t, dst, rpm)) {
				c.t.Errorf("%s differs between the source and the copy", rpm)
			}
		}
		c.cli("verify", dst)
		c.cli("check", dst, "--level", "fetch", "--arch", "any")
	}},

	{name: "copy_existing_destination_needs_a_flag", run: func(c *tctx, r rpmSet) {
		c.add(r.hello)
		dst := c.h.RepoURL(c.t)
		c.cli("copy", c.repo, dst)

		res := c.cliErr("copy", c.repo, dst)
		if !strings.Contains(res.combined(), "--continue") {
			c.t.Errorf("the refusal should offer --continue:\n%s", res.combined())
		}
		// Resuming skips the packages already transferred.
		res = c.cli("copy", c.repo, dst, "--continue")
		if !strings.Contains(res.combined(), "skip") {
			c.t.Errorf("--continue re-transferred everything:\n%s", res.combined())
		}
		c.cli("verify", dst)
	}},

	{name: "copy_filtered_rebuilds_metadata", run: func(c *tctx, r rpmSet) {
		c.add(r.hello, r.libfoo)
		dst := c.h.RepoURL(c.t)
		c.cli("copy", c.repo, dst, "--include", "hello")

		list := c.cli("list", dst).combined()
		if !strings.Contains(list, "hello") || strings.Contains(list, "libfoo") {
			c.t.Errorf("--include hello copied the wrong packages:\n%s", list)
		}
		if c.h.Exists(c.t, dst, loc(r.libfoo)) {
			c.t.Error("an excluded package's RPM was still uploaded")
		}
		c.cli("verify", dst)
	}},
}
