package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/danudey/createrepo-go/pkg/repoconfig"
)

// unmanaged publishes the reference repository without the config file this
// tool records, leaving a repository that looks like one somebody else created
// — which is what takeover is for.
func unmanaged(t *testing.T, dir string) {
	t.Helper()
	makeRepo(t, dir, "")
	if err := os.Remove(filepath.Join(dir, repoconfig.Path)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// listing returns every file under dir, repo-relative and sorted, so a test can
// prove an analysis wrote nothing.
func listing(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestTakeoverReportsACleanRepositoryAsSafe(t *testing.T) {
	dir := t.TempDir()
	unmanaged(t, dir)
	before := listing(t, dir)

	out, err := runCLI(t, "takeover", dir)
	if err != nil {
		t.Fatalf("takeover: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Verdict: 0 blocking") {
		t.Errorf("a repository with nothing unusual should have no blocking findings:\n%s", out)
	}
	if !strings.Contains(out, "safe to republish") {
		t.Errorf("the verdict is missing from the report:\n%s", out)
	}
	if got := listing(t, dir); !equal(got, before) {
		t.Errorf("takeover changed the repository:\nbefore %v\nafter  %v", before, got)
	}
}

func TestTakeoverExitsNonZeroWhenItWouldBreakTheRepository(t *testing.T) {
	dir := t.TempDir()
	unmanaged(t, dir)
	// A signed repository that the republish would not re-sign: the detached
	// signature stops matching repomd.xml, and clients with repo_gpgcheck=1
	// fail outright.
	sig := filepath.Join(dir, "repodata", "repomd.xml.asc")
	if err := os.WriteFile(sig, []byte("-----BEGIN PGP SIGNATURE-----\n\nx\n-----END PGP SIGNATURE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, "takeover", dir)
	if err == nil {
		t.Fatalf("takeover should exit non-zero while a blocking finding stands:\n%s", out)
	}
	if !strings.Contains(out, "metadata-signature-invalidated") {
		t.Errorf("the report does not name the problem:\n%s", out)
	}
	if !strings.Contains(out, "not safe to republish") {
		t.Errorf("the verdict does not say the takeover is unsafe:\n%s", out)
	}
}

func TestTakeoverAdoptRecordsSettingsAndPublishesNothing(t *testing.T) {
	dir := t.TempDir()
	unmanaged(t, dir)
	before := listing(t, dir)

	out, err := runCLI(t, "takeover", dir, "--adopt", "--yes",
		"--repo-name", "Legacy EL8", "--repo-url", "https://downloads.example.com/el8", "--target", "rhel8")
	if err != nil {
		t.Fatalf("takeover --adopt: %v\n%s", err, out)
	}

	data, err := os.ReadFile(filepath.Join(dir, repoconfig.Path))
	if err != nil {
		t.Fatalf("--adopt did not record the settings: %v", err)
	}
	var cfg repoconfig.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "Legacy EL8" || cfg.BaseURL != "https://downloads.example.com/el8" || cfg.Target != "rhel8" {
		t.Errorf("recorded config = %+v, want the name, URL and target that were given", cfg)
	}
	if cfg.LocationPrefix != "Packages" {
		t.Errorf("recorded location prefix = %q, want the directory the packages already live in", cfg.LocationPrefix)
	}

	// Nothing but the config file may appear: adopting claims the repository,
	// it does not republish it.
	want := append(append([]string{}, before...), repoconfig.Path)
	sort.Strings(want)
	if got := listing(t, dir); !equal(got, want) {
		t.Errorf("--adopt wrote more than the config file:\nwant %v\ngot  %v", want, got)
	}
}

func TestTakeoverJSONCarriesTheWholeReport(t *testing.T) {
	dir := t.TempDir()
	unmanaged(t, dir)

	out, err := runCLI(t, "takeover", dir, "--json")
	if err != nil {
		t.Fatalf("takeover --json: %v\n%s", err, out)
	}
	var rep struct {
		Location  string `json:"location"`
		Managed   bool   `json:"already_managed"`
		Published struct {
			Packages int `json:"packages"`
			Metadata []struct {
				Type string `json:"type"`
				Fate string `json:"fate"`
			} `json:"metadata"`
		} `json:"published"`
		Findings []struct {
			Severity string `json:"severity"`
			Code     string `json:"code"`
		} `json:"findings"`
		Config repoconfig.Config `json:"proposed_config"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("--json did not emit parseable JSON: %v\n%s", err, out)
	}
	if rep.Location != dir || rep.Published.Packages != 2 || rep.Managed {
		t.Errorf("JSON report does not describe the repository: %+v", rep)
	}
	if len(rep.Published.Metadata) != 3 {
		t.Errorf("expected the three generated documents, got %+v", rep.Published.Metadata)
	}
	for _, f := range rep.Findings {
		switch f.Severity {
		case "blocking", "advisory", "info":
		default:
			t.Errorf("finding %s carries an unknown severity %q", f.Code, f.Severity)
		}
	}
}

func TestTakeoverRefusesALocationWithNoRepository(t *testing.T) {
	out, err := runCLI(t, "takeover", t.TempDir())
	if err == nil {
		t.Errorf("takeover of an empty directory should fail:\n%s", out)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
