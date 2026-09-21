package takeover

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/danudey/createrepo-go/pkg/repo"
	"github.com/danudey/createrepo-go/pkg/repoconfig"
	"github.com/danudey/createrepo-go/pkg/repodata"
	"github.com/danudey/createrepo-go/pkg/rpmmeta"
)

const refRPMDir = "../../reference/rpmbuild/RPMS"

func referenceRPMs() []string {
	return []string{
		filepath.Join(refRPMDir, "noarch/hello-2.10-3.noarch.rpm"),
		filepath.Join(refRPMDir, "x86_64/libfoo-1.3.0-1.x86_64.rpm"),
	}
}

// extraDoc is a metadata document this tool does not generate, of the kind a
// repository built by createrepo_c carries.
type extraDoc struct {
	typ  string
	name string
	body string
}

// foreign describes a repository laid out the way other tooling lays one out:
// packages wherever that tooling put them, metadata under plain (not
// checksum-named) files, and documents this tool does not write.
type foreign struct {
	prefix       string // directory the packages live in ("" = repository root)
	checksumType string // package checksum recorded in primary (default sha256)
	extras       []extraDoc
	signature    string   // repodata/repomd.xml.asc contents, if signed
	strays       []string // extra files to leave lying around
	// mutate adjusts the package metadata before it is published, standing in
	// for metadata that has drifted from the files it describes.
	mutate func(*repodata.Package)
}

// build writes the repository to a fresh directory and returns its path.
func (f foreign) build(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	var pkgs []*repodata.Package
	for _, src := range referenceRPMs() {
		dst := filepath.Join(dir, f.prefix, filepath.Base(src))
		mkdirAll(t, filepath.Dir(dst))
		writeFile(t, dst, readFile(t, src))

		p, err := rpmmeta.FromFile(dst, rpmmeta.DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		p.Location = filepath.Base(src)
		if f.prefix != "" {
			p.Location = f.prefix + "/" + p.Location
		}
		if f.checksumType == "sha512" {
			sum := sha512.Sum512(readFile(t, dst))
			p.ChecksumType = "sha512"
			p.PkgID = hex.EncodeToString(sum[:])
		}
		if f.mutate != nil {
			f.mutate(p)
		}
		pkgs = append(pkgs, p)
	}

	repomd := &repodata.Repomd{Revision: "1700000000"}
	for _, d := range []struct {
		typ  string
		body []byte
	}{
		{"primary", repodata.WritePrimary(pkgs)},
		{"filelists", repodata.WriteFilelists(pkgs)},
		{"other", repodata.WriteOther(pkgs)},
	} {
		href := "repodata/" + d.typ + ".xml"
		writeFile(t, filepath.Join(dir, href), d.body)
		repomd.Data = append(repomd.Data, dataEntry(d.typ, href, d.body))
	}
	for _, e := range f.extras {
		href := "repodata/" + e.name
		writeFile(t, filepath.Join(dir, href), []byte(e.body))
		repomd.Data = append(repomd.Data, dataEntry(e.typ, href, []byte(e.body)))
	}
	writeFile(t, filepath.Join(dir, "repodata/repomd.xml"), repodata.WriteRepomd(repomd))
	if f.signature != "" {
		writeFile(t, filepath.Join(dir, "repodata/repomd.xml.asc"), []byte(f.signature))
	}
	for _, s := range f.strays {
		p := filepath.Join(dir, s)
		mkdirAll(t, filepath.Dir(p))
		writeFile(t, p, []byte("stray\n"))
	}
	return dir
}

// dataEntry builds the repomd record for an uncompressed document. The
// checksums are the real ones, so the repository is internally consistent.
func dataEntry(typ, href string, body []byte) repodata.DataEntry {
	sum := sha512.Sum512_256(body) // any real digest will do for the fixture
	hexSum := hex.EncodeToString(sum[:])
	return repodata.DataEntry{
		Type: typ, ChecksumType: "sha256", Checksum: hexSum, OpenChecksum: hexSum,
		Location: href, Timestamp: 1700000000, Size: int64(len(body)), OpenSize: int64(len(body)),
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	mkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// analyze opens dir and runs the analysis with the given settings, defaulting
// the ones every caller would otherwise repeat.
func analyze(t *testing.T, dir string, opt Options) *Report {
	t.Helper()
	if opt.Compression == "" {
		opt.Compression = "gzip"
	}
	if opt.ChecksumType == "" {
		opt.ChecksumType = "sha256"
	}
	r, err := repo.Open(context.Background(), dir, repo.Options{
		Compression:    repo.Compression(opt.Compression),
		ChecksumType:   opt.ChecksumType,
		LocationPrefix: opt.LocationPrefix,
		ChangelogLimit: opt.ChangelogLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	rep, err := Analyze(context.Background(), r, opt)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	return rep
}

// finding returns the findings carrying the given code.
func finding(rep *Report, code string) []Finding {
	var out []Finding
	for _, f := range rep.Findings {
		if f.Code == code {
			out = append(out, f)
		}
	}
	return out
}

// requireFinding fails unless exactly one finding with the code is present at
// the expected severity, and returns it.
func requireFinding(t *testing.T, rep *Report, code string, want Severity) Finding {
	t.Helper()
	found := finding(rep, code)
	if len(found) != 1 {
		t.Fatalf("expected one %q finding, got %d:\n%s", code, len(found), render(rep))
	}
	if found[0].Severity != want {
		t.Errorf("%q is %s, want %s", code, found[0].Severity, want)
	}
	return found[0]
}

// requireNoFinding fails when a finding with the code is present.
func requireNoFinding(t *testing.T, rep *Report, code string) {
	t.Helper()
	if f := finding(rep, code); len(f) > 0 {
		t.Errorf("unexpected %q finding: %s\n%s", code, f[0].Summary, render(rep))
	}
}

// render summarizes a report for a failure message.
func render(rep *Report) string {
	var b strings.Builder
	for _, f := range rep.Findings {
		fmt.Fprintf(&b, "  %s %s: %s\n", f.Severity, f.Code, f.Summary)
	}
	return b.String()
}

func TestAnalyzeReportsMetadataItWouldDrop(t *testing.T) {
	dir := foreign{
		prefix: "Packages",
		extras: []extraDoc{
			{typ: "group", name: "comps.xml", body: "<comps/>"},
			{typ: "updateinfo", name: "updateinfo.xml", body: "<updates/>"},
			{typ: "primary_db", name: "primary.sqlite", body: "sqlite"},
			{typ: "modules", name: "modules.yaml", body: "document: modulemd"},
		},
	}.build(t)

	rep := analyze(t, dir, Options{LocationPrefix: "Packages"})

	dropped := finding(rep, "metadata-dropped")
	if len(dropped) != 4 {
		t.Fatalf("expected four kinds of dropped metadata, got %d:\n%s", len(dropped), render(rep))
	}
	var blocking, advisory int
	for _, f := range dropped {
		switch f.Severity {
		case Blocking:
			blocking++
		case Advisory:
			advisory++
		case Info:
		}
	}
	// comps, updateinfo and modulemd break a client that uses them; sqlite does
	// not, because nothing on RHEL 8+ reads it.
	if blocking != 3 || advisory != 1 {
		t.Errorf("dropped metadata ranked %d blocking / %d advisory, want 3 / 1:\n%s", blocking, advisory, render(rep))
	}
	if rep.OK() {
		t.Error("a repository that would lose its comps and updateinfo metadata reported as safe")
	}

	// The files are named, and named as being deleted, so the operator can see
	// exactly what goes.
	var deleted []string
	deleted = append(deleted, rep.Changes.DeletedFiles...)
	for _, want := range []string{"repodata/comps.xml", "repodata/updateinfo.xml", "repodata/modules.yaml", "repodata/primary.sqlite"} {
		if !slices.Contains(deleted, want) {
			t.Errorf("%s is dropped but not listed among the files that would be deleted: %v", want, deleted)
		}
	}
	for _, m := range rep.Published.Metadata {
		want := "deleted"
		if m.Type == "primary" || m.Type == "filelists" || m.Type == "other" {
			want = "regenerated"
		}
		if m.Fate != want {
			t.Errorf("metadata %s fate = %q, want %q", m.Type, m.Fate, want)
		}
	}
}

func TestAnalyzeCleanRepositoryIsSafe(t *testing.T) {
	dir := foreign{prefix: "Packages"}.build(t)

	rep := analyze(t, dir, Options{LocationPrefix: "Packages"})

	if !rep.OK() {
		t.Errorf("a plain repository with nothing unusual reported as unsafe:\n%s", render(rep))
	}
	if rep.Published.Packages != 2 {
		t.Errorf("found %d packages, want 2", rep.Published.Packages)
	}
	if rep.Changes.Uploads != 0 {
		t.Errorf("a takeover would upload %d file(s); it should transfer nothing", rep.Changes.Uploads)
	}
	requireNoFinding(t, rep, "location-prefix-mismatch")
}

func TestAnalyzeRefusesForeignPackageChecksums(t *testing.T) {
	dir := foreign{prefix: "Packages", checksumType: "sha512"}.build(t)

	// Taken at its word, a repository indexed with another algorithm is a trap:
	// every later checksum comparison this tool makes fails.
	rep := analyze(t, dir, Options{LocationPrefix: "Packages"})
	f := requireFinding(t, rep, "package-checksum-type", Blocking)
	if !strings.Contains(f.Fix, "--from-packages") {
		t.Errorf("the finding should point at --from-packages, got %q", f.Fix)
	}

	// Re-reading the RPMs resolves it: the pkgids are recomputed, which is a
	// change worth reporting but not a breakage.
	rep = analyze(t, dir, Options{LocationPrefix: "Packages", FromPackages: true})
	requireFinding(t, rep, "package-checksum-type", Advisory)
	requireNoFinding(t, rep, "metadata-does-not-match-packages")
	if !rep.OK() {
		t.Errorf("re-reading the packages should make this repository safe to take over:\n%s", render(rep))
	}
}

func TestAnalyzeDetectsMetadataThatDoesNotMatchTheFiles(t *testing.T) {
	dir := foreign{
		prefix: "Packages",
		mutate: func(p *repodata.Package) { p.SizePackage += 4096 },
	}.build(t)

	rep := analyze(t, dir, Options{LocationPrefix: "Packages", FromPackages: true})

	f := requireFinding(t, rep, "metadata-does-not-match-packages", Blocking)
	if len(f.Items) != 2 {
		t.Errorf("expected both packages named as mismatched, got %v", f.Items)
	}
	if rep.Changes.Corrected != 2 {
		t.Errorf("reported %d corrected package(s), want 2", rep.Changes.Corrected)
	}
	// And the diff says which field it is, rather than only that something moved.
	if !diffCovers(rep, "package size") {
		t.Errorf("the package-size difference is not in the metadata diff: %+v", rep.Diff)
	}
}

func TestAnalyzeReportsAnInvalidatedMetadataSignature(t *testing.T) {
	signed := foreign{prefix: "Packages", signature: "-----BEGIN PGP SIGNATURE-----\n\nx\n-----END PGP SIGNATURE-----\n"}
	dir := signed.build(t)

	rep := analyze(t, dir, Options{LocationPrefix: "Packages"})

	f := requireFinding(t, rep, "metadata-signature-invalidated", Blocking)
	if !strings.Contains(f.Fix, "--sign-metadata") {
		t.Errorf("the finding should point at --sign-metadata, got %q", f.Fix)
	}
	if !rep.Published.SignedMetadata {
		t.Error("the report does not record that the repository is signed")
	}

	// Signing the republished metadata resolves it.
	rep = analyze(t, dir, Options{LocationPrefix: "Packages", SignMetadata: true, GPGKeyID: "test@example.com"})
	requireNoFinding(t, rep, "metadata-signature-invalidated")
}

func TestAnalyzeFindsFilesTheMetadataDoesNotAccountFor(t *testing.T) {
	dir := foreign{prefix: "Packages", strays: []string{"Packages/orphan.rpm", "repodata/primary.xml.old", "index.html"}}.build(t)

	rep := analyze(t, dir, Options{LocationPrefix: "Packages"})

	requireFinding(t, rep, "unreferenced-rpms", Info)
	requireFinding(t, rep, "stale-metadata", Info)
	requireFinding(t, rep, "other-files", Info)
	if got := rep.Published.StrayRPMs; len(got) != 1 || got[0] != "Packages/orphan.rpm" {
		t.Errorf("stray RPMs = %v, want [Packages/orphan.rpm]", got)
	}
	if !rep.OK() {
		t.Errorf("files stored alongside a repository do not make a takeover unsafe:\n%s", render(rep))
	}
}

func TestAnalyzeReportsPackagesTheRepositoryDoesNotHold(t *testing.T) {
	dir := foreign{prefix: "Packages"}.build(t)
	if err := os.Remove(filepath.Join(dir, "Packages/hello-2.10-3.noarch.rpm")); err != nil {
		t.Fatal(err)
	}

	rep := analyze(t, dir, Options{LocationPrefix: "Packages"})

	f := requireFinding(t, rep, "missing-packages", Blocking)
	if !slices.Contains(f.Items, "Packages/hello-2.10-3.noarch.rpm") {
		t.Errorf("the missing package is not named: %v", f.Items)
	}
}

func TestAnalyzeRelocatesPackagesServerSide(t *testing.T) {
	dir := foreign{}.build(t) // packages at the repository root

	// Without --location-prefix the packages stay where they are, and the
	// mismatch with where later additions would land is reported instead.
	rep := analyze(t, dir, Options{LocationPrefix: "Packages"})
	requireFinding(t, rep, "location-prefix-mismatch", Advisory)
	if rep.Changes.Relocated != 0 {
		t.Errorf("packages were relocated without --location-prefix being given")
	}

	// With it, every package moves, and on a backend that can copy server-side
	// nothing is uploaded.
	rep = analyze(t, dir, Options{LocationPrefix: "Packages", Relocate: true})
	if rep.Changes.Relocated != 2 || rep.Changes.ServerSideMoves != 2 {
		t.Errorf("relocated %d package(s) with %d server-side move(s), want 2 and 2",
			rep.Changes.Relocated, rep.Changes.ServerSideMoves)
	}
	if rep.Changes.Uploads != 0 {
		t.Errorf("a server-side relocation should upload nothing, got %d upload(s)", rep.Changes.Uploads)
	}
	requireFinding(t, rep, "packages-relocated", Advisory)
	if !diffCovers(rep, "location") {
		t.Errorf("the location change is not in the metadata diff: %+v", rep.Diff)
	}
}

func TestProposedConfigRecordsWhereThePackagesAlreadyAre(t *testing.T) {
	dir := foreign{prefix: "rpms"}.build(t)

	rep := analyze(t, dir, Options{
		LocationPrefix: "Packages", // the default, not given explicitly
		Name:           "Legacy EL8",
		BaseURL:        "https://downloads.example.com/el8",
		Target:         "rhel8",
	})

	want := repoconfig.Config{
		Name: "Legacy EL8", BaseURL: "https://downloads.example.com/el8",
		LocationPrefix: "rpms", Target: "rhel8",
	}
	if rep.Config != want {
		t.Errorf("proposed config = %+v, want %+v", rep.Config, want)
	}
}

func TestProposedConfigKeepsFieldsItDoesNotManage(t *testing.T) {
	dir := foreign{prefix: "Packages"}.build(t)
	prev := &repoconfig.Config{Name: "Kept", CopySource: "https://upstream.example.com/el9"}

	rep := analyze(t, dir, Options{LocationPrefix: "Packages", ExistingConfig: prev})

	if rep.Config.CopySource != prev.CopySource || rep.Config.Name != "Kept" {
		t.Errorf("proposed config lost fields it does not manage: %+v", rep.Config)
	}
	if !rep.Managed {
		t.Error("a repository with a recorded config should be reported as already managed")
	}
}

func TestAnalyzeWithoutRereadingSaysSo(t *testing.T) {
	dir := foreign{prefix: "Packages"}.build(t)

	rep := analyze(t, dir, Options{LocationPrefix: "Packages"})

	f := requireFinding(t, rep, "metadata-unchecked", Advisory)
	if !strings.Contains(f.Fix, "--from-packages") {
		t.Errorf("the finding should name the flag that resolves it, got %q", f.Fix)
	}
}

func TestAnalyzeRejectsALocationWithNoRepository(t *testing.T) {
	dir := t.TempDir()
	r, err := repo.Open(context.Background(), dir, repo.Options{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if _, err := Analyze(context.Background(), r, Options{}); err == nil {
		t.Error("analysing an empty location should fail: there is nothing to take over")
	}
}

// diffCovers reports whether the metadata diff mentions the named field.
func diffCovers(rep *Report, field string) bool {
	for _, d := range rep.Diff {
		if d.Field == field {
			return true
		}
	}
	return false
}
