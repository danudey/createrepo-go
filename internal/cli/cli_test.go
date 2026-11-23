package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestEnvBool(t *testing.T) {
	const name = "CREATEREPO_TEST_BOOL"
	for _, v := range []string{"1", "true", "TRUE", "yes", " on "} {
		t.Setenv(name, v)
		if !EnvBool(name) {
			t.Errorf("EnvBool(%q) = false; want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "maybe"} {
		t.Setenv(name, v)
		if EnvBool(name) {
			t.Errorf("EnvBool(%q) = true; want false", v)
		}
	}
}

func TestSplitList(t *testing.T) {
	if got := SplitList(" a , b ,, c "); strings.Join(got, "|") != "a|b|c" {
		t.Errorf("SplitList trimmed or split wrongly: %v", got)
	}
	if got := SplitList(""); len(got) != 0 {
		t.Errorf(`SplitList("") = %v; want nothing`, got)
	}
}

func TestPromptYesNo(t *testing.T) {
	for in, want := range map[string]bool{"y\n": true, "YES\n": true, "n\n": false, "": false, "sure\n": false} {
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetIn(strings.NewReader(in))
		if got := PromptYesNo(cmd, "Proceed?"); got != want {
			t.Errorf("answer %q: PromptYesNo = %v, want %v", in, got, want)
		}
		if out.String() != "Proceed? [y/N] " {
			t.Errorf("prompt = %q", out.String())
		}
	}
}
