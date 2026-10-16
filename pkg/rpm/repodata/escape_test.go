package repodata

import (
	"encoding/xml"
	"strings"
	"testing"
)

// TestEscapedOutputIsAlwaysWellFormed feeds the writer text no package should
// contain and checks that what comes out is still XML a strict parser reads.
func TestEscapedOutputIsAlwaysWellFormed(t *testing.T) {
	inputs := []string{
		"plain",
		"a & b < c > d \" e ' f ]]> g",
		"control \x01\x02\x1b[2J chars",
		"invalid \xe9 utf-8",
		"invalid \xe9 utf-8 with & an escape",
		"surrogate \xed\xa0\x80 bytes",
		"non-characters ￾ ￿",
		"line\r\nendings\tand tabs",
		"<!ENTITY x SYSTEM \"file:///etc/passwd\">",
	}
	for _, in := range inputs {
		doc := "<r a=\"" + escapeAttr(in) + "\">" + escapeText(in) + "</r>"
		var v struct {
			A    string `xml:"a,attr"`
			Text string `xml:",chardata"`
		}
		if err := xml.Unmarshal([]byte(doc), &v); err != nil {
			t.Errorf("%q: produced invalid XML %q: %v", in, doc, err)
			continue
		}
		// Whitespace in an attribute must survive the parser's attribute
		// normalization.
		if xmlSafe(in) && v.A != in {
			t.Errorf("%q: attribute did not round-trip: %q", in, v.A)
		}
	}
}

// TestValidTextIsWrittenUnchanged guards the createrepo_c golden output: text
// with nothing to escape must come out byte for byte.
func TestValidTextIsWrittenUnchanged(t *testing.T) {
	for _, in := range []string{"hello", "naïve café", "multi\nline \"quoted\" text", "emoji 🎉"} {
		if got := escapeText(in); got != in {
			t.Errorf("escapeText(%q) = %q", in, got)
		}
	}
}

// xmlSafe reports whether s is valid UTF-8 holding only characters XML allows,
// i.e. whether it can round-trip at all.
func xmlSafe(s string) bool {
	for _, r := range s {
		if !xmlChar(r) || r == '�' {
			return false
		}
	}
	return strings.ToValidUTF8(s, "") == s
}

// Fields copied verbatim from another repository's metadata must not be able
// to add markup to the metadata this tool writes. Before they were escaped, a
// hostile pkgid could close <checksum> and add a <location xml:base> pointing
// clients at another host.
func TestCopiedFieldsCannotInjectMarkup(t *testing.T) {
	const inject = `x"/><location xml:base="http://evil.example" href="x.rpm"/><y a="`
	p := &Package{
		Name:         "hello",
		Arch:         "noarch",
		Version:      "1",
		Release:      "1",
		PkgID:        "abc</checksum>" + inject,
		ChecksumType: "sha256" + inject,
		Location:     "Packages/hello.rpm",
		Provides:     []Entry{{Name: "hello", Flags: "EQ" + inject, Ver: "1"}},
		Files:        []File{{Path: "/usr/bin/hello", Type: "dir" + inject}},
	}

	got, err := ParsePrimary(WritePrimary([]*Package{p}))
	if err != nil {
		t.Fatalf("primary.xml does not parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("primary.xml holds %d packages, want 1", len(got))
	}
	g := got[0]
	if g.XMLBase != "" || g.Location != p.Location {
		t.Errorf("location = (%q, %q), want (\"\", %q)", g.XMLBase, g.Location, p.Location)
	}
	if g.PkgID != p.PkgID || g.ChecksumType != p.ChecksumType {
		t.Errorf("checksum = (%q, %q), want (%q, %q)", g.ChecksumType, g.PkgID, p.ChecksumType, p.PkgID)
	}
	if len(g.Provides) != 1 || g.Provides[0].Flags != p.Provides[0].Flags {
		t.Errorf("provides = %+v, want %+v", g.Provides, p.Provides)
	}

	files, err := ParseFilelists(WriteFilelists([]*Package{p}))
	if err != nil {
		t.Fatalf("filelists.xml does not parse: %v", err)
	}
	if f := files[p.PkgID]; len(f) != 1 || f[0] != p.Files[0] {
		t.Errorf("filelists for %q = %+v, want %+v", p.PkgID, f, p.Files)
	}
	if _, err := ParseOther(WriteOther([]*Package{p})); err != nil {
		t.Errorf("other.xml does not parse: %v", err)
	}

	d := DataEntry{
		Type:         "primary" + inject,
		ChecksumType: "sha256" + inject,
		Checksum:     "abc</checksum>" + inject,
		OpenChecksum: "def</open-checksum>" + inject,
		Location:     "repodata/primary.xml.gz",
	}
	r, err := ParseRepomd(WriteRepomd(&Repomd{Revision: "1", Data: []DataEntry{d}}))
	if err != nil {
		t.Fatalf("repomd.xml does not parse: %v", err)
	}
	if len(r.Data) != 1 {
		t.Fatalf("repomd.xml holds %d data entries, want 1", len(r.Data))
	}
	if r.Data[0].Type != d.Type || r.Data[0].Checksum != d.Checksum ||
		r.Data[0].OpenChecksum != d.OpenChecksum || r.Data[0].ChecksumType != d.ChecksumType {
		t.Errorf("repomd data = %+v, want %+v", r.Data[0], d)
	}
}
