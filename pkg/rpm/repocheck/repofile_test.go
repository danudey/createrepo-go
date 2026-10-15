// Copyright (c) 2026 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package repocheck

import "testing"

func TestParseRepoFile(t *testing.T) {
	content := `
# a comment
[base]
name=Base
baseurl=https://example.com/repo/$releasever/$basearch/
enabled=1

[disabled]
name=Disabled
baseurl=https://example.com/off/
enabled=0

[mirror]
name=Mirror
mirrorlist=https://example.com/mirrors
`
	secs, err := parseRepoFile(content)
	if err != nil {
		t.Fatalf("parseRepoFile: %v", err)
	}
	if len(secs) != 3 {
		t.Fatalf("got %d sections, want 3", len(secs))
	}
	if secs[0].ID != "base" || secs[0].BaseURL != "https://example.com/repo/$releasever/$basearch/" {
		t.Errorf("unexpected base section: %+v", secs[0])
	}
	if secs[1].Enabled {
		t.Errorf("disabled section should not be enabled")
	}
	if secs[2].MirrorList == "" {
		t.Errorf("mirror section should record mirrorlist")
	}
}

func TestParseRepoFileNoSections(t *testing.T) {
	if _, err := parseRepoFile("just some text\nno stanzas\n"); err == nil {
		t.Errorf("expected error for content with no sections")
	}
}

func TestSubstituteVars(t *testing.T) {
	got := substituteVars("https://h/$releasever/${basearch}/os/$arch", "9", "x86_64")
	want := "https://h/9/x86_64/os/x86_64"
	if got != want {
		t.Errorf("substituteVars = %q, want %q", got, want)
	}
}

func TestExpandTargets(t *testing.T) {
	secs := []repoSection{
		{ID: "rel", BaseURL: "https://h/$releasever/", Enabled: true},
		{ID: "arch", BaseURL: "https://h/os/$basearch/", Enabled: true},
		{ID: "plain", BaseURL: "https://h/plain", Enabled: true},
		{ID: "off", BaseURL: "https://h/off/", Enabled: false},
	}
	targets, warns := expandTargets(secs, []string{"8", "9"}, []string{"x86_64", "aarch64"})

	// rel -> 2 (8,9); arch -> 2 (x86_64,aarch64); plain -> 1; off -> 0.
	if len(targets) != 5 {
		t.Fatalf("got %d targets, want 5:\n%+v", len(targets), targets)
	}
	// Disabled section should produce a warning.
	if len(warns) == 0 {
		t.Errorf("expected a warning for the disabled section")
	}
	// Every expanded baseurl must end in a slash.
	for _, tg := range targets {
		if tg.baseURL[len(tg.baseURL)-1] != '/' {
			t.Errorf("baseURL %q not slash-terminated", tg.baseURL)
		}
	}
	// The plain section keeps its bare label.
	var sawPlain bool
	for _, tg := range targets {
		if tg.label == "plain" && tg.baseURL == "https://h/plain/" {
			sawPlain = true
		}
	}
	if !sawPlain {
		t.Errorf("plain target not found in %+v", targets)
	}
}

func TestExpandTargetsMissingReleasever(t *testing.T) {
	secs := []repoSection{{ID: "rel", BaseURL: "https://h/$releasever/", Enabled: true}}
	targets, warns := expandTargets(secs, nil, nil)
	if len(targets) != 0 {
		t.Errorf("expected no targets when releasever is required but absent")
	}
	if len(warns) == 0 {
		t.Errorf("expected a warning when releasever is required but absent")
	}
}

func TestLoadRepoInputBareLocation(t *testing.T) {
	secs, err := loadRepoInput("s3://bucket/prefix", nil)
	if err != nil {
		t.Fatalf("loadRepoInput: %v", err)
	}
	if len(secs) != 1 || secs[0].BaseURL != "s3://bucket/prefix" {
		t.Errorf("bare location not passed through: %+v", secs)
	}
}

func TestLoadRepoInputHTTPRepoFile(t *testing.T) {
	fetched := false
	fetch := func(u string) ([]byte, error) {
		fetched = true
		return []byte("[r]\nbaseurl=https://h/repo/\n"), nil
	}
	secs, err := loadRepoInput("https://h/my.repo", fetch)
	if err != nil {
		t.Fatalf("loadRepoInput: %v", err)
	}
	if !fetched {
		t.Errorf("expected the .repo URL to be fetched")
	}
	if len(secs) != 1 || secs[0].BaseURL != "https://h/repo/" {
		t.Errorf("unexpected sections: %+v", secs)
	}
}

func TestPathOf(t *testing.T) {
	cases := map[string]string{
		"https://h/a/b.repo": "/a/b.repo",
		"/local/path/c.repo": "/local/path/c.repo",
		"relative/d.repo":    "relative/d.repo",
		"s3://bucket/e.repo": "/e.repo",
	}
	for in, want := range cases {
		if got := pathOf(in); got != want {
			t.Errorf("pathOf(%q) = %q, want %q", in, got, want)
		}
	}
}
