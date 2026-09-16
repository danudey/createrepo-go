package repo

import (
	"testing"

	"github.com/danudey/createrepo-go/pkg/repodata"
)

func mkPkg(name, arch, ver, rel string) *repodata.Package {
	return &repodata.Package{Name: name, Arch: arch, Version: ver, Release: rel}
}

func TestRPMVerCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0", "1.0", 0},
		{"1.0", "1.1", -1},
		{"1.1", "1.0", 1},
		{"1.10", "1.9", 1},   // numeric, not lexical
		{"1.0", "1.0.1", -1}, // extra segment wins
		{"2", "1", 1},
		{"1.0", "1.0a", -1},    // alpha segment after digits
		{"1.0a", "1.0", 1},     // alpha segment present vs absent
		{"1.0~rc1", "1.0", -1}, // tilde sorts before release
		{"1.0", "1.0~rc1", 1},  // and the reverse
		{"1.0~rc1", "1.0~rc2", -1},
		{"fc38", "fc39", -1},
		{"00010", "10", 0}, // leading zeros ignored
	}
	for _, c := range cases {
		if got := rpmVerCompare(c.a, c.b); got != c.want {
			t.Errorf("rpmVerCompare(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestPruneOlderUsesVersionCompare(t *testing.T) {
	older := mkPkg("foo", "x86_64", "1.9", "1")
	newer := mkPkg("foo", "x86_64", "1.10", "1")
	if !rpmEVRLess(older, newer) {
		t.Errorf("1.9-1 should sort below 1.10-1")
	}
	if rpmEVRLess(newer, older) {
		t.Errorf("1.10-1 should not sort below 1.9-1")
	}
}
