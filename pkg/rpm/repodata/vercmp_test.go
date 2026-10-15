package repodata

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0", "1.0", 0},
		{"1.0", "1.1", -1},
		{"1.10", "1.9", 1}, // numeric, not lexical
		{"1.0", "1.0.1", -1},
		{"1.0~rc1", "1.0", -1}, // tilde sorts before
		{"1.0", "1.0~rc1", 1},  // and the reverse
		{"1.0~rc1", "1.0~rc2", -1},
		{"1.0^post", "1.0", 1}, // caret sorts after a bare base
		{"1.0", "1.0^post", -1},
		{"00010", "10", 0}, // leading zeros ignored
		{"fc38", "fc39", -1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareEVR(t *testing.T) {
	// Epoch dominates version.
	if CompareEVR(1, "1.0", "1", 0, "9.9", "1") <= 0 {
		t.Errorf("higher epoch should win regardless of version")
	}
	// Equal epoch falls back to version then release.
	if CompareEVR(0, "1.0", "2", 0, "1.0", "10") >= 0 {
		t.Errorf("release 2 should sort below release 10")
	}
	if CompareEVR(0, "1.0", "1", 0, "1.0", "1") != 0 {
		t.Errorf("identical EVR should compare equal")
	}
}
