package repofile

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const sample = `# a comment
[base]
name=Example $releasever
baseurl=https://example.com/$releasever/${basearch}/
  https://mirror.example.com/$releasever/$basearch/
gpgcheck=1
repo_gpgcheck=True
gpgkey=https://example.com/RPM-GPG-KEY-$releasever,
       file:///etc/pki/rpm-gpg/RPM-GPG-KEY-extra

[off]
baseurl=https://example.com/off/
enabled=0
pkg_gpgcheck=yes

[mirrored]
metalink=https://example.com/metalink?repo=$releasever
`

func TestParse(t *testing.T) {
	got, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	want := []Section{
		{
			ID: "base", Name: "Example $releasever", Enabled: true, GPGCheck: true, RepoGPGCheck: true,
			BaseURLs: []string{"https://example.com/$releasever/${basearch}/", "https://mirror.example.com/$releasever/$basearch/"},
			GPGKeys:  []string{"https://example.com/RPM-GPG-KEY-$releasever", "file:///etc/pki/rpm-gpg/RPM-GPG-KEY-extra"},
		},
		{ID: "off", BaseURLs: []string{"https://example.com/off/"}, GPGCheck: true},
		{ID: "mirrored", Enabled: true, MirrorList: "https://example.com/metalink?repo=$releasever"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Parse =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseRejectsMalformedFiles(t *testing.T) {
	for name, data := range map[string]string{
		"no sections":         "baseurl=https://example.com/\n",
		"key before section":  "gpgcheck=1\n[a]\n",
		"bad boolean":         "[a]\ngpgcheck=maybe\n",
		"not key=value":       "[a]\njunk\n",
		"orphan continuation": "[a]\n  https://example.com/\n",
	} {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: Parse accepted %q", name, data)
		}
	}
}

func TestExpand(t *testing.T) {
	vars := map[string]string{"releasever": "9", "basearch": "x86_64"}
	got, err := Expand("https://example.com/$releasever/${basearch}/$releasever", vars)
	if err != nil || got != "https://example.com/9/x86_64/9" {
		t.Errorf("Expand = %q, %v", got, err)
	}
	if got, err := Expand("https://example.com/a$/b", nil); err != nil || got != "https://example.com/a$/b" {
		t.Errorf("a lone $ should be left alone: %q, %v", got, err)
	}

	_, err = Expand("https://example.com/$releasever/$arch/$contentdir", map[string]string{"releasever": "9"})
	var missing *MissingVarsError
	if !errors.As(err, &missing) || !reflect.DeepEqual(missing.Names, []string{"$arch", "$contentdir"}) {
		t.Fatalf("Expand = %v, want the missing variables named", err)
	}
}

func TestSectionVars(t *testing.T) {
	secs, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := secs[0].Vars(), map[string]bool{"releasever": true, "basearch": true}; !reflect.DeepEqual(got, want) {
		t.Errorf("Vars = %v, want %v", got, want)
	}
	if got := secs[1].Vars(); len(got) != 0 {
		t.Errorf("Vars of a section with no variables = %v", got)
	}
}

func TestSectionExpandCoversKeys(t *testing.T) {
	secs, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secs[0].Expand(map[string]string{"basearch": "x86_64"}); err == nil || !strings.Contains(err.Error(), "[base]") {
		t.Errorf("Expand without $releasever = %v, want an error naming the section", err)
	}
	got, err := secs[0].Expand(map[string]string{"releasever": "9", "basearch": "x86_64"})
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURLs[0] != "https://example.com/9/x86_64/" || got.GPGKeys[0] != "https://example.com/RPM-GPG-KEY-9" {
		t.Errorf("Expand = %+v", got)
	}
}
