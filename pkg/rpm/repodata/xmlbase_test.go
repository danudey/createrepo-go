package repodata

import (
	"strings"
	"testing"
)

// A primary.xml fragment as createrepo writes it for a repository that serves
// its packages from another host.
const primaryWithBase = `<?xml version="1.0" encoding="UTF-8"?>
<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm" packages="1">
<package type="rpm">
  <name>hello</name>
  <arch>noarch</arch>
  <version epoch="0" ver="2.10" rel="3"/>
  <checksum type="sha256" pkgid="YES">abc123</checksum>
  <summary>s</summary>
  <description>d</description>
  <packager></packager>
  <url></url>
  <time file="1" build="1"/>
  <size package="10" installed="10" archive="10"/>
  <location xml:base="https://upstream.example.com/el9/" href="Packages/hello-2.10-3.noarch.rpm"/>
  <format>
    <rpm:header-range start="1" end="2"/>
  </format>
</package>
</metadata>
`

func TestParsePrimaryReadsXMLBase(t *testing.T) {
	pkgs, err := ParsePrimary([]byte(primaryWithBase))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("parsed %d packages, want 1", len(pkgs))
	}
	if got, want := pkgs[0].XMLBase, "https://upstream.example.com/el9/"; got != want {
		t.Errorf("XMLBase = %q, want %q", got, want)
	}
	if got, want := pkgs[0].Location, "Packages/hello-2.10-3.noarch.rpm"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}

func TestWritePrimaryRoundTripsXMLBase(t *testing.T) {
	pkgs, err := ParsePrimary([]byte(primaryWithBase))
	if err != nil {
		t.Fatal(err)
	}
	out := string(WritePrimary(pkgs))
	if !strings.Contains(out, `<location xml:base="https://upstream.example.com/el9/" href="Packages/hello-2.10-3.noarch.rpm"/>`) {
		t.Errorf("xml:base was not written back:\n%s", out)
	}

	// Clearing it — what `copy --remove-baseurl` does — leaves a plain location.
	pkgs[0].XMLBase = ""
	out = string(WritePrimary(pkgs))
	if strings.Contains(out, "xml:base") {
		t.Errorf("xml:base survived being cleared:\n%s", out)
	}
	if !strings.Contains(out, `<location href="Packages/hello-2.10-3.noarch.rpm"/>`) {
		t.Errorf("plain location not written:\n%s", out)
	}
}
