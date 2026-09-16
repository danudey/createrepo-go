package repodata

import (
	"strings"
	"testing"
)

func fpkg(name, arch, ver string) *Package {
	return &Package{Name: name, Arch: arch, Version: ver, Release: "1",
		PkgID: name + "-" + ver + "." + arch, Location: "Packages/" + name + "-" + ver + "-1." + arch + ".rpm"}
}

// sample is a small repository covering every kind and two versions of one
// package, so one fixture exercises all the filter dimensions.
func sample() []*Package {
	return []*Package{
		fpkg("hello", "noarch", "2.10"),
		fpkg("libfoo", "x86_64", "1.3.0"),
		fpkg("libfoo", "x86_64", "1.2.0"),
		fpkg("libfoo", "aarch64", "1.3.0"),
		fpkg("libfoo-debuginfo", "x86_64", "1.3.0"),
		fpkg("libfoo-debugsource", "x86_64", "1.3.0"),
		fpkg("libfoo", "src", "1.3.0"),
	}
}

func names(pkgs []*Package) []string {
	out := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, p.NEVRA())
	}
	return out
}

func applyOK(t *testing.T, f Filter, pkgs []*Package) []*Package {
	t.Helper()
	got, _, err := f.Apply(pkgs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return got
}

func TestKindOf(t *testing.T) {
	cases := map[*Package]Kind{
		fpkg("libfoo", "x86_64", "1"):             KindBinary,
		fpkg("libfoo", "src", "1"):                KindSource,
		fpkg("libfoo-debuginfo", "x86_64", "1"):   KindDebuginfo,
		fpkg("libfoo-debugsource", "x86_64", "1"): KindDebugsource,
		// A source rpm of a debug package is still classified as source: the
		// architecture decides first.
		fpkg("libfoo-debuginfo", "src", "1"): KindSource,
	}
	for p, want := range cases {
		if got := KindOf(p); got != want {
			t.Errorf("KindOf(%s) = %s, want %s", p.NEVRA(), got, want)
		}
	}
}

func TestParseKindsDebugAlias(t *testing.T) {
	got, err := ParseKinds([]string{"debug"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != KindDebuginfo || got[1] != KindDebugsource {
		t.Errorf("ParseKinds(debug) = %v, want [debuginfo debugsource]", got)
	}
	if _, err := ParseKinds([]string{"nonsense"}); err == nil {
		t.Error("expected an error for an unknown kind")
	}
}

func TestFilterEmptySelectsEverything(t *testing.T) {
	pkgs := sample()
	var f Filter
	if !f.Empty() {
		t.Fatal("a zero Filter should report itself empty")
	}
	if got := applyOK(t, f, pkgs); len(got) != len(pkgs) {
		t.Errorf("selected %d packages, want all %d", len(got), len(pkgs))
	}
}

func TestFilterArchKeepsNoarchAndRejectsMissing(t *testing.T) {
	got := applyOK(t, Filter{Arches: []string{"x86_64"}}, sample())
	for _, p := range got {
		if p.Arch != "x86_64" && p.Arch != "noarch" {
			t.Errorf("%s slipped through an x86_64-only filter", p.NEVRA())
		}
	}
	var sawNoarch bool
	for _, p := range got {
		if p.Name == "hello" {
			sawNoarch = true
		}
	}
	if !sawNoarch {
		t.Error("the noarch package was dropped; a single-arch copy still needs it")
	}

	_, _, err := Filter{Arches: []string{"s390x"}}.Apply(sample())
	if err == nil {
		t.Fatal("expected an error for an architecture the repository does not have")
	}
	if !strings.Contains(err.Error(), "s390x") {
		t.Errorf("error should name the missing arch, got: %v", err)
	}
}

func TestFilterKinds(t *testing.T) {
	binOnly := applyOK(t, Filter{Kinds: []Kind{KindBinary}}, sample())
	for _, p := range binOnly {
		if KindOf(p) != KindBinary {
			t.Errorf("%s is not a binary package", p.NEVRA())
		}
	}
	if len(binOnly) != 4 { // hello, libfoo x2 x86_64, libfoo aarch64
		t.Errorf("binary-only selection has %d packages: %v", len(binOnly), names(binOnly))
	}

	noDebug := applyOK(t, Filter{ExcludeKinds: []Kind{KindDebuginfo, KindDebugsource}}, sample())
	for _, p := range noDebug {
		if strings.Contains(p.Name, "-debug") {
			t.Errorf("%s survived --exclude-kinds debug", p.NEVRA())
		}
	}
}

func TestFilterIncludeExcludePatterns(t *testing.T) {
	cases := []struct {
		name    string
		filter  Filter
		wantAll []string
	}{
		{"bare name", Filter{Include: []string{"hello"}}, []string{"hello-2.10-1.noarch"}},
		{"name glob", Filter{Include: []string{"libfoo-debug*"}},
			[]string{"libfoo-debuginfo-1.3.0-1.x86_64", "libfoo-debugsource-1.3.0-1.x86_64"}},
		{"name-version", Filter{Include: []string{"libfoo-1.2.0"}}, []string{"libfoo-1.2.0-1.x86_64"}},
		{"version glob", Filter{Include: []string{"libfoo-1.2.*"}}, []string{"libfoo-1.2.0-1.x86_64"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := names(applyOK(t, tc.filter, sample()))
			if len(got) != len(tc.wantAll) {
				t.Fatalf("got %v, want %v", got, tc.wantAll)
			}
			for i := range got {
				if got[i] != tc.wantAll[i] {
					t.Errorf("got %v, want %v", got, tc.wantAll)
					break
				}
			}
		})
	}

	// Exclude wins over include for a package both patterns match.
	got := names(applyOK(t, Filter{Include: []string{"libfoo*"}, Exclude: []string{"*-debuginfo"}}, sample()))
	for _, n := range got {
		if strings.HasPrefix(n, "libfoo-debuginfo") {
			t.Errorf("exclude did not override include: %v", got)
		}
	}
}

func TestFilterWarnsOnPatternThatMatchesNothing(t *testing.T) {
	_, warnings, err := Filter{Include: []string{"hello", "nosuchpkg"}}.Apply(sample())
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "nosuchpkg") {
		t.Errorf("expected one warning naming nosuchpkg, got %v", warnings)
	}
}

func TestFilterLatestOnly(t *testing.T) {
	got := applyOK(t, Filter{LatestOnly: true, Kinds: []Kind{KindBinary}}, sample())
	for _, p := range got {
		if p.Name == "libfoo" && p.Arch == "x86_64" && p.Version != "1.3.0" {
			t.Errorf("--latest-only kept superseded %s", p.NEVRA())
		}
	}
	if len(got) != 3 { // hello, libfoo x86_64 1.3.0, libfoo aarch64 1.3.0
		t.Errorf("got %d packages, want 3: %v", len(got), names(got))
	}
}
