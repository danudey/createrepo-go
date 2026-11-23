package sources

import (
	"strings"
	"testing"
)

func TestParseListSources(t *testing.T) {
	doc := `# a comment
deb [arch=amd64 signed-by=/etc/apt/keyrings/x.asc] https://deb.example.com/apt bookworm main contrib
deb-src https://deb.example.com/apt bookworm main

deb https://deb.example.com/apt flat/
`
	entries, warnings, err := parseListSources([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries; want 2: %+v", len(entries), entries)
	}
	if entries[0].URI != "https://deb.example.com/apt" || entries[0].Suite != "bookworm" {
		t.Errorf("first entry = %+v", entries[0])
	}
	if strings.Join(entries[0].Components, ",") != "main,contrib" {
		t.Errorf("components = %v; want main and contrib", entries[0].Components)
	}
	if strings.Join(entries[0].Arches, ",") != "amd64" {
		t.Errorf("arches = %v; want amd64 from the options group", entries[0].Arches)
	}
	if !entries[1].Source {
		t.Error("the deb-src entry was not marked as a source entry")
	}
	// The trivial (flat) layout is refused, with a warning naming the line.
	if len(warnings) != 1 || !strings.Contains(warnings[0], "flat") {
		t.Errorf("warnings = %v; want one about the flat layout", warnings)
	}
}

func TestParseDeb822Sources(t *testing.T) {
	doc := `Types: deb deb-src
URIs: https://deb.example.com/apt
Suites: bookworm trixie
Components: main
Architectures: amd64 arm64

Types: deb
URIs: https://other.example.com/apt
Suites: stable
Components: main
Enabled: no
`
	entries, _, err := parseDeb822Sources([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	// Two types times two suites, and the disabled stanza is skipped.
	if len(entries) != 4 {
		t.Fatalf("got %d entries; want 4: %+v", len(entries), entries)
	}
	for _, e := range entries {
		if e.URI != "https://deb.example.com/apt" {
			t.Errorf("the disabled stanza was not skipped: %+v", e)
		}
	}
}
