package repodata

import (
	"sort"
	"testing"
)

// dep builds a versioned dependency entry (flags like "EQ", "GE", ...).
func dep(name, flags, ver, rel string) Entry {
	return Entry{Name: name, Flags: flags, Ver: ver, Rel: rel}
}

// tpkg builds a package with the given name/version and dependency entries.
func tpkg(name, ver, rel string, provides, requires []Entry) *Package {
	return &Package{
		Name: name, Arch: "x86_64", Version: ver, Release: rel,
		PkgID:    name + "-" + ver + "-" + rel,
		Provides: provides,
		Requires: requires,
	}
}

func TestConstraint(t *testing.T) {
	cases := []struct {
		e    Entry
		want string
	}{
		{Entry{Name: "bash"}, "bash"},
		{dep("bash", "GE", "4.0", ""), "bash >= 4.0"},
		{dep("foo", "EQ", "1.2", "3"), "foo = 1.2-3"},
		{Entry{Name: "foo", Flags: "LT", Epoch: 2, Ver: "1.0"}, "foo < 2:1.0"},
	}
	for _, c := range cases {
		if got := c.e.Constraint(); got != c.want {
			t.Errorf("Constraint(%+v) = %q; want %q", c.e, got, c.want)
		}
	}
}

func TestEntrySatisfies(t *testing.T) {
	cases := []struct {
		name      string
		prov, req Entry
		want      bool
	}{
		{"unversioned require matches any provide", dep("a", "EQ", "1.0", "1"), Entry{Name: "a"}, true},
		{"unversioned provide matches any require", Entry{Name: "a"}, dep("a", "GE", "2.0", ""), true},
		{"exact version match", dep("a", "EQ", "1.0", "1"), dep("a", "EQ", "1.0", ""), true},
		{"exact version mismatch", dep("a", "EQ", "1.0", "1"), dep("a", "EQ", "1.2", ""), false},
		{"GE satisfied by newer", dep("a", "EQ", "1.2", "1"), dep("a", "GE", "1.0", ""), true},
		{"GE not satisfied by older", dep("a", "EQ", "1.0", "1"), dep("a", "GE", "1.2", ""), false},
		{"LE satisfied by older", dep("a", "EQ", "1.0", "1"), dep("a", "LE", "1.2", ""), true},
		{"GT boundary excluded", dep("a", "EQ", "1.0", "1"), dep("a", "GT", "1.0", ""), false},
		{"release ignored when require omits it", dep("a", "EQ", "1.2", "5"), dep("a", "EQ", "1.2", ""), true},
		{"epoch dominates", Entry{Name: "a", Flags: "EQ", Epoch: 1, Ver: "1.0"}, dep("a", "GE", "9.9", ""), true},
	}
	for _, c := range cases {
		if got := entrySatisfies(c.prov, c.req); got != c.want {
			t.Errorf("%s: entrySatisfies(%s, %s) = %v; want %v",
				c.name, c.prov.Constraint(), c.req.Constraint(), got, c.want)
		}
	}
}

func TestCheckDependencies(t *testing.T) {
	// B requires A >= 1.2 but only A-1.0 is present: an unmet intra-repo dep.
	a10 := tpkg("A", "1.0", "1", []Entry{dep("A", "EQ", "1.0", "1")}, nil)
	b := tpkg("B", "1.0", "1", nil, []Entry{
		dep("A", "GE", "1.2", ""),
		dep("glibc", "GE", "2.17", ""), // external: no package provides glibc, skipped
		{Name: "rpmlib(PayloadIsZstd)", Flags: "LE", Ver: "5.4.18", Rel: "1"}, // skipped
	})
	problems := CheckDependencies([]*Package{a10, b})
	if len(problems) != 1 {
		t.Fatalf("got %d problems, want 1: %v", len(problems), problems)
	}
	if problems[0].Package != b || problems[0].Requires.Name != "A" {
		t.Errorf("unexpected problem: %s", problems[0])
	}

	// With A-1.2 present as well, the requirement is satisfied.
	a12 := tpkg("A", "1.2", "1", []Entry{dep("A", "EQ", "1.2", "1")}, nil)
	if problems := CheckDependencies([]*Package{a10, a12, b}); len(problems) != 0 {
		t.Errorf("expected no problems once A-1.2 present, got: %v", problems)
	}
}

func TestCheckDependenciesFileAndVirtual(t *testing.T) {
	// A provides a virtual capability and a file; B requires both.
	a := &Package{
		Name: "A", Arch: "x86_64", Version: "1.0", Release: "1", PkgID: "A",
		Provides: []Entry{dep("A", "EQ", "1.0", "1"), {Name: "webserver"}},
		Files:    []File{{Path: "/usr/bin/serve"}},
	}
	b := tpkg("B", "1.0", "1", nil, []Entry{{Name: "webserver"}, {Name: "/usr/bin/serve"}})
	if problems := CheckDependencies([]*Package{a, b}); len(problems) != 0 {
		t.Errorf("virtual/file deps should be satisfied, got: %v", problems)
	}
	// Remove the file provider and B's file requirement becomes unmet, but only
	// because some package (A) still lists it? No — nothing provides it now, so
	// it is treated as external and not reported.
	a.Files = nil
	if problems := CheckDependencies([]*Package{a, b}); len(problems) != 0 {
		t.Errorf("unprovided file dep is external, got: %v", problems)
	}
}

func TestRemovalBreakages(t *testing.T) {
	// A has 1.0 and 1.2; B requires A = 1.0 specifically.
	a10 := tpkg("A", "1.0", "1", []Entry{dep("A", "EQ", "1.0", "1")}, nil)
	a12 := tpkg("A", "1.2", "1", []Entry{dep("A", "EQ", "1.2", "1")}, nil)
	b := tpkg("B", "1.0", "1", nil, []Entry{dep("A", "EQ", "1.0", "")})
	all := []*Package{a10, a12, b}

	// Pruning the older versions (A-1.0) would break B: A-1.0 must be protected.
	br := RemovalBreakages(all, []*Package{a10})
	if len(br) != 1 {
		t.Fatalf("got %d breakages, want 1: %v", len(br), br)
	}
	if br[0].Provider != a10 || br[0].Dependent != b {
		t.Errorf("unexpected breakage: %s", br[0])
	}

	// If B instead requires A >= 1.0, A-1.2 still satisfies it, so removing A-1.0
	// breaks nothing.
	b.Requires = []Entry{dep("A", "GE", "1.0", "")}
	if br := RemovalBreakages(all, []*Package{a10}); len(br) != 0 {
		t.Errorf("A>=1.0 is satisfied by A-1.2; want no breakage, got: %v", br)
	}

	// A dependent that is itself being removed does not count.
	b.Requires = []Entry{dep("A", "EQ", "1.0", "")}
	if br := RemovalBreakages(all, []*Package{a10, b}); len(br) != 0 {
		t.Errorf("dependent B is also removed; want no breakage, got: %v", br)
	}
}

func TestRemovalBreakagesMultipleCandidates(t *testing.T) {
	// A-1.0, A-1.1, A-1.2 all present; B pins A = 1.0. Pruning both older
	// versions should flag only A-1.0 as required (A-1.1 is free to go).
	a10 := tpkg("A", "1.0", "1", []Entry{dep("A", "EQ", "1.0", "1")}, nil)
	a11 := tpkg("A", "1.1", "1", []Entry{dep("A", "EQ", "1.1", "1")}, nil)
	a12 := tpkg("A", "1.2", "1", []Entry{dep("A", "EQ", "1.2", "1")}, nil)
	b := tpkg("B", "1.0", "1", nil, []Entry{dep("A", "EQ", "1.0", "")})
	all := []*Package{a10, a11, a12, b}

	br := RemovalBreakages(all, []*Package{a10, a11})
	var providers []string
	for _, x := range br {
		providers = append(providers, x.Provider.PkgID)
	}
	sort.Strings(providers)
	if len(providers) != 1 || providers[0] != "A-1.0-1" {
		t.Errorf("want only A-1.0 protected, got %v", providers)
	}
}
