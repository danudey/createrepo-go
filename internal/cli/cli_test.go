package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/danudey/createrepo-go/pkg/checkcore"
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

func TestReportCheckPutsFailuresFirstAndReportsTheResult(t *testing.T) {
	var out bytes.Buffer
	failed := ReportCheck(&out, []checkcore.Result{
		{Kind: "fine", Status: checkcore.StatusOK},
		{Kind: "odd", Status: checkcore.StatusWarn},
		{Kind: "broken", Status: checkcore.StatusFail},
	})
	got := out.String()
	if !failed || !strings.Contains(got, "RESULT: FAIL") {
		t.Errorf("ReportCheck = %v, output %q; want a failure", failed, got)
	}
	if strings.Index(got, "broken") > strings.Index(got, "odd") {
		t.Errorf("the failure is not listed before the warning:\n%s", got)
	}
	if !strings.Contains(got, "Summary: 1 ok, 1 failed, 1 warnings, 0 skipped") {
		t.Errorf("summary missing or wrong:\n%s", got)
	}
}
