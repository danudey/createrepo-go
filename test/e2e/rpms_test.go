//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// rpmSet holds the RPM fixtures used by the scenarios. The two base packages
// are the committed reference RPMs; helloNewer is built on demand (when
// rpmbuild is available) so update/prune/coexistence can be exercised.
type rpmSet struct {
	hello      string // hello-2.10-3.noarch.rpm  (reference fixture)
	libfoo     string // libfoo-1.3.0-1.x86_64.rpm (reference fixture)
	helloNewer string // hello-2.10-4.noarch.rpm  (built; "" if rpmbuild absent)

	// Dependency fixtures (built when rpmbuild is present, "" otherwise):
	// depApp requires "deplib = 1.0" specifically, so deplib-1.0 must not be
	// pruned when deplib-2.0 is added.
	depApp    string // depapp-1.0-1.noarch.rpm
	depLibOld string // deplib-1.0-1.noarch.rpm
	depLibNew string // deplib-2.0-1.noarch.rpm
}

const refRPMDir = "../../reference/rpmbuild/RPMS"
const specDir = "../../reference/specs"

// haveNewer reports whether the bumped hello build is available.
func (r rpmSet) haveNewer() bool { return r.helloNewer != "" }

// haveDep reports whether the dependency fixtures are available.
func (r rpmSet) haveDep() bool { return r.depApp != "" && r.depLibOld != "" && r.depLibNew != "" }

// buildRPMs locates the reference RPMs and, if rpmbuild is present, builds a
// newer release of hello (2.10-4) into outDir.
func buildRPMs(outDir string) (rpmSet, error) {
	rs := rpmSet{
		hello:  filepath.Join(refRPMDir, "noarch", "hello-2.10-3.noarch.rpm"),
		libfoo: filepath.Join(refRPMDir, "x86_64", "libfoo-1.3.0-1.x86_64.rpm"),
	}
	for _, p := range []string{rs.hello, rs.libfoo} {
		if _, err := os.Stat(p); err != nil {
			return rs, err
		}
	}
	if _, ok := haveExec("rpmbuild"); !ok {
		return rs, nil // base fixtures only
	}

	// Bump hello to release 4 by editing a copy of the spec.
	spec, err := os.ReadFile(filepath.Join(specDir, "hello.spec"))
	if err != nil {
		return rs, err
	}
	bumped := strings.Replace(string(spec),
		"Release:        3%{?dist}", "Release:        4%{?dist}", 1)
	bumped = strings.Replace(bumped, "%changelog",
		"%changelog\n* Tue Jun 02 2026 Tigera Test <test@tigera.io> - 2.10-4\n- Fourth build for e2e update tests",
		1)
	specPath := filepath.Join(outDir, "hello-newer.spec")
	if err := os.WriteFile(specPath, []byte(bumped), 0o644); err != nil {
		return rs, err
	}
	cmd := exec.Command("rpmbuild",
		"--define", "_topdir "+outDir,
		"--define", "dist %{nil}",
		"-bb", specPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return rs, err // surfaces the rpmbuild output
	} else {
		_ = out
	}
	built := filepath.Join(outDir, "RPMS", "noarch", "hello-2.10-4.noarch.rpm")
	if _, err := os.Stat(built); err != nil {
		return rs, err
	}
	rs.helloNewer = built

	// Dependency fixtures: two versions of deplib and an app pinning the old one.
	if err := buildDepFixtures(outDir, &rs); err != nil {
		return rs, err
	}
	return rs, nil
}

// buildDepFixtures builds deplib-1.0, deplib-2.0 and depapp (which requires
// "deplib = 1.0") into outDir, populating the dependency fields of rs.
func buildDepFixtures(outDir string, rs *rpmSet) error {
	specs := map[string]string{
		"deplib-1.0": depSpec("deplib", "1.0", ""),
		"deplib-2.0": depSpec("deplib", "2.0", ""),
		"depapp-1.0": depSpec("depapp", "1.0", "Requires:       deplib = 1.0"),
	}
	for name, spec := range specs {
		specPath := filepath.Join(outDir, name+".spec")
		if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
			return err
		}
		cmd := exec.Command("rpmbuild",
			"--define", "_topdir "+outDir,
			"--define", "dist %{nil}",
			"-bb", specPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return err
		} else {
			_ = out
		}
	}
	rs.depLibOld = filepath.Join(outDir, "RPMS", "noarch", "deplib-1.0-1.noarch.rpm")
	rs.depLibNew = filepath.Join(outDir, "RPMS", "noarch", "deplib-2.0-1.noarch.rpm")
	rs.depApp = filepath.Join(outDir, "RPMS", "noarch", "depapp-1.0-1.noarch.rpm")
	for _, p := range []string{rs.depLibOld, rs.depLibNew, rs.depApp} {
		if _, err := os.Stat(p); err != nil {
			return err
		}
	}
	return nil
}

// depSpec renders a minimal noarch spec for a package with an optional extra
// tag line (e.g. a Requires:).
func depSpec(name, version, extra string) string {
	return "Name:           " + name + "\n" +
		"Version:        " + version + "\n" +
		"Release:        1\n" +
		"Summary:        " + name + " dependency fixture\n" +
		"License:        MIT\n" +
		"BuildArch:      noarch\n" +
		extra + "\n" +
		"%description\n" + name + " dependency fixture\n" +
		"%files\n"
}

// rpmBase returns the basename a package gets under the repo root (no prefix).
func rpmBase(t *testing.T, path string) string {
	t.Helper()
	return filepath.Base(path)
}
