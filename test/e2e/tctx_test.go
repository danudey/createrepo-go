//go:build e2e

package e2e

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// tctx is the per-scenario context: a fresh repository on one backend, plus
// convenience wrappers for invoking the CLI and asserting on results.
type tctx struct {
	t        *testing.T
	h        Harness
	repo     string   // fresh repository URL for this scenario
	extraEnv []string // appended to the harness env (e.g. GNUPGHOME for signing)
}

func newTctx(t *testing.T, h Harness) *tctx {
	return &tctx{t: t, h: h, repo: h.RepoURL(t)}
}

func (c *tctx) env() []string {
	return append(append([]string{}, c.h.Env()...), c.extraEnv...)
}

// cli runs the CLI and fails the test if it exits non-zero.
func (c *tctx) cli(args ...string) cliResult {
	c.t.Helper()
	res := runCLI(c.t, c.env(), args...)
	if res.err != nil {
		c.t.Fatalf("createrepo-go %s: unexpected failure: %v\n%s",
			strings.Join(args, " "), res.err, res.combined())
	}
	return res
}

// cliInput runs the CLI feeding input on stdin (e.g. to answer a confirmation
// prompt) and fails the test if it exits non-zero.
func (c *tctx) cliInput(input string, args ...string) cliResult {
	c.t.Helper()
	res := runCLIStdin(c.t, c.env(), input, args...)
	if res.err != nil {
		c.t.Fatalf("createrepo-go %s: unexpected failure: %v\n%s",
			strings.Join(args, " "), res.err, res.combined())
	}
	return res
}

// cliErr runs the CLI expecting a non-zero exit, returning the result.
func (c *tctx) cliErr(args ...string) cliResult {
	c.t.Helper()
	res := runCLI(c.t, c.env(), args...)
	if res.err == nil {
		c.t.Fatalf("createrepo-go %s: expected failure, got success\n%s",
			strings.Join(args, " "), res.combined())
	}
	return res
}

// --- repository convenience operations --------------------------------------

func (c *tctx) create() cliResult { return c.cli("create", c.repo) }
func (c *tctx) add(rpms ...string) cliResult {
	return c.cli(append([]string{"add", c.repo}, rpms...)...)
}
func (c *tctx) list() cliResult      { return c.cli("list", c.repo) }
func (c *tctx) verify() cliResult    { return c.cli("verify", c.repo) }
func (c *tctx) verifyBad() cliResult { return c.cliErr("verify", c.repo) }

// check runs the deep validator at the given level. --arch any avoids
// filtering by the host architecture so both the noarch and x86_64 fixtures
// are always checked.
func (c *tctx) check(level string) cliResult {
	return c.cli("check", "--arch", "any", "--level", level, c.repo)
}

// checkBad runs check at the given level expecting a non-zero exit.
func (c *tctx) checkBad(level string) cliResult {
	return c.cliErr("check", "--arch", "any", "--level", level, c.repo)
}

// checkOK asserts that check at the given level reports RESULT: OK.
func (c *tctx) checkOK(level string) {
	c.t.Helper()
	res := c.check(level)
	if !strings.Contains(res.stdout, "RESULT: OK") {
		c.t.Errorf("check --level %s did not report OK:\n%s", level, res.combined())
	}
}

// addWith runs add with extra flags before the rpm arguments.
func (c *tctx) addWith(flags []string, rpms ...string) cliResult {
	args := append([]string{"add", c.repo}, flags...)
	return c.cli(append(args, rpms...)...)
}

// --- assertions -------------------------------------------------------------

var pkgCountRE = regexp.MustCompile(`(?m)^(\d+) package\(s\)`)

// listCount parses the trailing "N package(s)" line from list/verify output.
func (c *tctx) listCount() int {
	c.t.Helper()
	out := c.list().stdout
	m := pkgCountRE.FindStringSubmatch(out)
	if m == nil {
		c.t.Fatalf("could not find package count in list output:\n%s", out)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func (c *tctx) wantCount(want int) {
	c.t.Helper()
	if got := c.listCount(); got != want {
		c.t.Errorf("repository has %d package(s), want %d", got, want)
	}
}

// wantListContains asserts the list output mentions each substring (e.g. a NEVRA).
func (c *tctx) wantListContains(subs ...string) {
	c.t.Helper()
	out := c.list().stdout
	for _, s := range subs {
		if !strings.Contains(out, s) {
			c.t.Errorf("list output missing %q:\n%s", s, out)
		}
	}
}

func (c *tctx) wantListExcludes(subs ...string) {
	c.t.Helper()
	out := c.list().stdout
	for _, s := range subs {
		if strings.Contains(out, s) {
			c.t.Errorf("list output unexpectedly contains %q:\n%s", s, out)
		}
	}
}

func (c *tctx) mustExist(relpath string) {
	c.t.Helper()
	if !c.h.Exists(c.t, c.repo, relpath) {
		c.t.Errorf("expected object %q to exist in %s", relpath, c.repo)
	}
}

func (c *tctx) mustMissing(relpath string) {
	c.t.Helper()
	if c.h.Exists(c.t, c.repo, relpath) {
		c.t.Errorf("expected object %q to be absent in %s", relpath, c.repo)
	}
}

// repodataFiles returns the repodata/ entries referenced by repomd.xml, by type.
// It reads repomd.xml from the backend and extracts location hrefs.
var locationRE = regexp.MustCompile(`<location href="([^"]+)"`)

func (c *tctx) repomdLocations() []string {
	c.t.Helper()
	data := c.h.ReadAll(c.t, c.repo, "repodata/repomd.xml")
	var out []string
	for _, m := range locationRE.FindAllStringSubmatch(string(data), -1) {
		out = append(out, m[1])
	}
	return out
}

// verifyOK runs verify and asserts the success summary is printed.
func (c *tctx) verifyOK() {
	c.t.Helper()
	res := c.verify()
	if !strings.Contains(res.stdout, "OK:") {
		c.t.Errorf("verify did not report OK:\n%s", res.combined())
	}
}
