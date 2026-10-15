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
