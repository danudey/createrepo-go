package aptdata

import "testing"

func TestValidPackageName(t *testing.T) {
	for _, name := range []string{"hello", "libfoo1", "g++-12", "python3.11", "x"} {
		if err := ValidPackageName(name); err != nil {
			t.Errorf("ValidPackageName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", ".", "..", "../../x", "a/b", "/etc", `a\b`, "a b", "a\nb"} {
		if err := ValidPackageName(name); err == nil {
			t.Errorf("ValidPackageName(%q) = nil, want an error", name)
		}
	}
}

func TestValidFileName(t *testing.T) {
	for _, name := range []string{"hello_2.10-3.dsc", "hello_2.10.orig.tar.gz", "x~1"} {
		if err := ValidFileName(name); err != nil {
			t.Errorf("ValidFileName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", ".", "..", "../x", "a/b", `a\b`} {
		if err := ValidFileName(name); err == nil {
			t.Errorf("ValidFileName(%q) = nil, want an error", name)
		}
	}
}
