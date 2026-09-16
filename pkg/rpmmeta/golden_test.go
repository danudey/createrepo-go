package rpmmeta

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/danudey/createrepo-go/pkg/repodata"
)

// TestMetadataMatchesCreaterepo generates primary/filelists/other for the
// reference RPMs and asserts that, after a write+parse round-trip, the result
// is semantically identical to what createrepo_c produced (ignoring the file
// mtime, which legitimately differs between runs).
func TestMetadataMatchesCreaterepo(t *testing.T) {
	rpms := []string{
		filepath.Join(referenceRepo, "hello-2.10-3.noarch.rpm"),
		filepath.Join(referenceRepo, "libfoo-1.3.0-1.x86_64.rpm"),
	}
	var ours []*repodata.Package
	for _, f := range rpms {
		p, err := FromFile(f, DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		ours = append(ours, p)
	}

	// Round-trip our generated metadata through the writers and parsers and
	// merge into a full package view, exactly as a client would.
	primary := repodata.WritePrimary(ours)
	filelists := repodata.WriteFilelists(ours)
	other := repodata.WriteOther(ours)

	oursParsed, err := repodata.ParsePrimary(primary)
	if err != nil {
		t.Fatal(err)
	}
	flMap, _ := repodata.ParseFilelists(filelists)
	otMap, _ := repodata.ParseOther(other)
	ourIdx := repodata.Merge(oursParsed, flMap, otMap)

	// Reference metadata from createrepo_c.
	refPrimary := readGzReference(t, "primary")
	refFilelists := readGzReference(t, "filelists")
	refOther := readGzReference(t, "other")
	refPkgs, err := repodata.ParsePrimary(refPrimary)
	if err != nil {
		t.Fatal(err)
	}
	refFl, _ := repodata.ParseFilelists(refFilelists)
	refOt, _ := repodata.ParseOther(refOther)
	refIdx := repodata.Merge(refPkgs, refFl, refOt)

	ourByName := byName(ourIdx.Packages())
	refByName := byName(refIdx.Packages())
	if len(ourByName) != len(refByName) {
		t.Fatalf("package count: ours=%d ref=%d", len(ourByName), len(refByName))
	}

	for name, ref := range refByName {
		got := ourByName[name]
		if got == nil {
			t.Errorf("missing package %s", name)
			continue
		}
		// Normalize the legitimately-varying field.
		ref.FileTime = 0
		got.FileTime = 0
		if !reflect.DeepEqual(got, ref) {
			t.Errorf("package %s differs:\n ours=%+v\n  ref=%+v", name, got, ref)
		}
	}
}

func byName(pkgs []*repodata.Package) map[string]*repodata.Package {
	m := make(map[string]*repodata.Package, len(pkgs))
	for _, p := range pkgs {
		m[p.Name] = p
	}
	return m
}
