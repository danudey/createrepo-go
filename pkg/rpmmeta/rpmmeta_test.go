package rpmmeta

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/danudey/createrepo-go/pkg/repodata"
)

// referenceRepo is the createrepo_c-generated repo built by the repo setup.
const referenceRepo = "../../reference/repo"

func TestFromFileHello(t *testing.T) {
	p, err := FromFile(filepath.Join(referenceRepo, "hello-2.10-3.noarch.rpm"), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "hello" || p.Arch != "noarch" || p.Version != "2.10" || p.Release != "3" {
		t.Errorf("identity wrong: %+v", p)
	}
	if p.HeaderStart != 4456 || p.HeaderEnd != 7303 {
		t.Errorf("header range = %d-%d, want 4456-7303", p.HeaderStart, p.HeaderEnd)
	}
	if p.SizeInstall != 35 {
		t.Errorf("installed size = %d, want 35", p.SizeInstall)
	}
	// Provides: greeting, hello (both EQ).
	if len(p.Provides) != 2 {
		t.Fatalf("provides = %+v", p.Provides)
	}
	// Requires excludes rpmlib(...) and keeps /bin/sh, bash (GE), coreutils.
	wantReq := map[string]string{"/bin/sh": "", "bash": "GE", "coreutils": ""}
	if len(p.Requires) != len(wantReq) {
		t.Fatalf("requires = %+v, want %v", p.Requires, wantReq)
	}
	for _, e := range p.Requires {
		if f, ok := wantReq[e.Name]; !ok || f != e.Flags {
			t.Errorf("requires entry %+v unexpected", e)
		}
	}
	// Files: 3 total, one dir.
	if len(p.Files) != 3 {
		t.Fatalf("files = %+v", p.Files)
	}
	var dirs int
	for _, f := range p.Files {
		if f.Type == "dir" {
			dirs++
		}
	}
	if dirs != 1 {
		t.Errorf("dir count = %d, want 1", dirs)
	}
	// Primary files: only /usr/bin/hello.
	pf := p.PrimaryFiles()
	if len(pf) != 1 || pf[0].Path != "/usr/bin/hello" {
		t.Errorf("primary files = %+v", pf)
	}
	// Changelogs oldest-first.
	if len(p.Changelogs) != 2 {
		t.Fatalf("changelogs = %+v", p.Changelogs)
	}
	if p.Changelogs[0].Date >= p.Changelogs[1].Date {
		t.Errorf("changelogs not oldest-first: %+v", p.Changelogs)
	}
}

// TestPkgIDMatchesReference confirms our package checksum equals the pkgid that
// createrepo_c recorded in primary.xml.
func TestPkgIDMatchesReference(t *testing.T) {
	refPrimary := readGzReference(t, "primary")
	refPkgs, err := repodata.ParsePrimary(refPrimary)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*repodata.Package{}
	for _, p := range refPkgs {
		byName[p.Name] = p
	}
	for _, rpmFile := range []string{"hello-2.10-3.noarch.rpm", "libfoo-1.3.0-1.x86_64.rpm"} {
		p, err := FromFile(filepath.Join(referenceRepo, rpmFile), DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		ref := byName[p.Name]
		if ref == nil {
			t.Fatalf("no reference package for %s", p.Name)
		}
		if p.PkgID != ref.PkgID {
			t.Errorf("%s pkgid = %s, want %s", p.Name, p.PkgID, ref.PkgID)
		}
		if p.HeaderStart != ref.HeaderStart || p.HeaderEnd != ref.HeaderEnd {
			t.Errorf("%s header range = %d-%d, want %d-%d", p.Name,
				p.HeaderStart, p.HeaderEnd, ref.HeaderStart, ref.HeaderEnd)
		}
	}
}

func readGzReference(t *testing.T, kind string) []byte {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(referenceRepo, "repodata", "*-"+kind+".xml.gz"))
	if len(matches) != 1 {
		t.Fatalf("expected one %s.xml.gz, found %v", kind, matches)
	}
	out, err := exec.Command("zcat", matches[0]).Output()
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(out)
}
