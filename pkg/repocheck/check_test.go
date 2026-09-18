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

import (
	"testing"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/repodata"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"metadata": LevelMetadata,
		"meta":     LevelMetadata,
		"head":     LevelHead,
		"exists":   LevelHead,
		"fetch":    LevelFetch,
		"full":     LevelFetch,
		"download": LevelFetch,
		"HEAD":     LevelHead,
	}
	for in, want := range cases {
		got, err := ParseLevel(in)
		if err != nil {
			t.Errorf("ParseLevel(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseLevel("bogus"); err == nil {
		t.Errorf("expected error for bogus level")
	}
}

func pkg(name, arch string, epoch int, ver, rel string) *repodata.Package {
	return &repodata.Package{Name: name, Arch: arch, Epoch: epoch, Version: ver, Release: rel}
}

func TestSelectPackagesFilters(t *testing.T) {
	all := []*repodata.Package{
		pkg("foo", "x86_64", 0, "1.0", "1"),
		pkg("foo", "aarch64", 0, "1.0", "1"),
		pkg("bar", "x86_64", 0, "2.0", "1"),
		pkg("baz", "noarch", 0, "3.0", "1"),
	}

	// Name filter.
	ck := newChecker(Config{Packages: []string{"foo"}}, RPMTool{}, nil)
	if got := ck.selectPackages(all); len(got) != 2 {
		t.Errorf("name filter: got %d, want 2", len(got))
	}

	// Arch filter keeps the matching arch plus noarch.
	ck = newChecker(Config{Arches: []string{"x86_64"}}, RPMTool{}, nil)
	got := ck.selectPackages(all)
	if len(got) != 3 { // foo.x86_64, bar.x86_64, baz.noarch
		t.Errorf("arch filter: got %d, want 3: %+v", len(got), names(got))
	}
	for _, p := range got {
		if p.Arch == "aarch64" {
			t.Errorf("arch filter should have dropped aarch64")
		}
	}
}

func TestSelectPackagesLatestOnly(t *testing.T) {
	all := []*repodata.Package{
		pkg("foo", "x86_64", 0, "1.9", "1"),
		pkg("foo", "x86_64", 0, "1.10", "1"), // newer (numeric compare)
		pkg("foo", "x86_64", 0, "1.0~rc1", "1"),
	}
	ck := newChecker(Config{LatestOnly: true}, RPMTool{}, nil)
	got := ck.selectPackages(all)
	if len(got) != 1 {
		t.Fatalf("latest-only: got %d, want 1", len(got))
	}
	if got[0].Version != "1.10" {
		t.Errorf("latest-only picked %s, want 1.10", got[0].Version)
	}
}

func names(pkgs []*repodata.Package) []string {
	out := make([]string, len(pkgs))
	for i, p := range pkgs {
		out[i] = p.NEVRA()
	}
	return out
}

// depOnlyBackend is a stub backend used to exercise checkDependencies, which
// only needs String() for the passing-case location.
type depOnlyBackend struct{ backend.Backend }

func (depOnlyBackend) String() string { return "test://repo" }

func TestCheckDependencies(t *testing.T) {
	be := depOnlyBackend{}

	// A pins libdep = 1.0, but only libdep-2.0 is present: a broken intra-repo dep.
	app := &repodata.Package{
		Name: "app", Arch: "noarch", Version: "1.0", Release: "1", Location: "Packages/app.rpm",
		Requires: []repodata.Entry{{Name: "libdep", Flags: "EQ", Ver: "1.0"}},
	}
	lib2 := &repodata.Package{
		Name: "libdep", Arch: "noarch", Version: "2.0", Release: "1",
		Provides: []repodata.Entry{{Name: "libdep", Flags: "EQ", Ver: "2.0", Rel: "1"}},
	}

	ck := newChecker(Config{}, RPMTool{}, nil)
	ck.checkDependencies("repo", be, []*repodata.Package{app, lib2})
	res := ck.Results()
	if len(res) != 1 || res[0].Status != StatusFail || res[0].Kind != "dependency" {
		t.Fatalf("expected one FAIL dependency result, got %+v", res)
	}

	// Add libdep-1.0 and the graph is satisfied: a single OK summary.
	lib1 := &repodata.Package{
		Name: "libdep", Arch: "noarch", Version: "1.0", Release: "1",
		Provides: []repodata.Entry{{Name: "libdep", Flags: "EQ", Ver: "1.0", Rel: "1"}},
	}
	ck = newChecker(Config{}, RPMTool{}, nil)
	ck.checkDependencies("repo", be, []*repodata.Package{app, lib1, lib2})
	res = ck.Results()
	if len(res) != 1 || res[0].Status != StatusOK || res[0].Kind != "dependencies" {
		t.Fatalf("expected one OK dependencies result, got %+v", res)
	}
}

func TestSummarize(t *testing.T) {
	res := []Result{
		{Status: StatusOK}, {Status: StatusOK}, {Status: StatusFail}, {Status: StatusWarn}, {Status: StatusSkip},
	}
	ok, fail, warn, skip := Summarize(res)
	if ok != 2 || fail != 1 || warn != 1 || skip != 1 {
		t.Errorf("Summarize = (%d,%d,%d,%d), want (2,1,1,1)", ok, fail, warn, skip)
	}
}
