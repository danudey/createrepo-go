package repo

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/rpm/repodata"
)

func checkMetadataDir(t *testing.T, dir string) *MetadataReport {
	t.Helper()
	be, err := backend.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return CheckMetadata(context.Background(), be)
}

// rewriteRepomd replaces repomd.xml with md, as a broken publish might.
func rewriteRepomd(t *testing.T, dir string, md *repodata.Repomd) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, repomdPath), repodata.WriteRepomd(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

// addDocument publishes an extra repodata document (comps, updateinfo, …)
// with a correct entry in repomd.xml, returning the entry.
func addDocument(t *testing.T, dir string, md *repodata.Repomd, typ string, open []byte) repodata.DataEntry {
	t.Helper()
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write(open)
	w.Close()
	sum := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	d := repodata.DataEntry{
		Type: typ, ChecksumType: "sha256", Checksum: sum(gz.Bytes()), OpenChecksum: sum(open),
		Location: "repodata/" + sum(gz.Bytes()) + "-" + typ + ".xml.gz", Size: int64(gz.Len()), OpenSize: int64(len(open)),
	}
	if err := os.WriteFile(filepath.Join(dir, d.Location), gz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	md.Data = append(md.Data, d)
	rewriteRepomd(t, dir, md)
	return d
}

func problemText(ps []Problem) string {
	var s []string
	for _, p := range ps {
		s = append(s, p.String())
	}
	return strings.Join(s, "\n")
}

func TestCheckMetadataPassesAHealthyRepository(t *testing.T) {
	dir, md := publishedRepo(t)
	addDocument(t, dir, md, "group", []byte("<comps/>"))
	rep := checkMetadataDir(t, dir)
	if len(rep.Problems) != 0 || len(rep.Warnings) != 0 || !rep.Usable() {
		t.Fatalf("problems:\n%s\nwarnings:\n%s", problemText(rep.Problems), problemText(rep.Warnings))
	}
	if len(rep.Files) != 4 {
		t.Errorf("checked %d documents, want 4 (primary, filelists, other, group)", len(rep.Files))
	}
	if len(rep.Cache) != 3 {
		t.Errorf("cached %d documents, want the 3 a Repo loads", len(rep.Cache))
	}
}

// Before verify absorbed check, a document verify does not load (comps,
// updateinfo, modules) could be damaged without verify noticing.
func TestCheckMetadataCatchesADamagedExtraDocument(t *testing.T) {
	dir, md := publishedRepo(t)
	d := addDocument(t, dir, md, "updateinfo", []byte("<updates/>"))
	corrupt(t, filepath.Join(dir, d.Location))

	rep := checkMetadataDir(t, dir)
	if got := problemText(rep.Problems); !strings.Contains(got, "updateinfo metadata") || !strings.Contains(got, "checksum") {
		t.Errorf("problems = %q, want the updateinfo checksum mismatch", got)
	}
	if !rep.Usable() {
		t.Error("a damaged updateinfo should not stop the packages being checked")
	}
}

func TestCheckMetadataReportsWhatStopsTheIndexLoading(t *testing.T) {
	for name, damage := range map[string]func(t *testing.T, dir string, md *repodata.Repomd){
		"corrupt primary": func(t *testing.T, dir string, md *repodata.Repomd) {
			corrupt(t, filepath.Join(dir, md.DataEntryByType("primary").Location))
		},
		"missing primary": func(t *testing.T, dir string, md *repodata.Repomd) {
			os.Remove(filepath.Join(dir, md.DataEntryByType("primary").Location))
		},
		"wrong open-checksum": func(t *testing.T, dir string, md *repodata.Repomd) {
			for i := range md.Data {
				if md.Data[i].Type == "primary" {
					md.Data[i].OpenChecksum = strings.Repeat("0", 64)
				}
			}
			rewriteRepomd(t, dir, md)
		},
		"no primary entry": func(t *testing.T, dir string, md *repodata.Repomd) {
			var kept []repodata.DataEntry
			for _, d := range md.Data {
				if d.Type != "primary" {
					kept = append(kept, d)
				}
			}
			md.Data = kept
			rewriteRepomd(t, dir, md)
		},
		"primary that does not parse": func(t *testing.T, dir string, md *repodata.Repomd) {
			for i, d := range md.Data {
				if d.Type == "primary" {
					os.Remove(filepath.Join(dir, d.Location))
					md.Data = append(md.Data[:i], md.Data[i+1:]...)
					addDocument(t, dir, md, "primary", []byte("<metadata><package"))
					break
				}
			}
		},
		"repomd.xml that does not parse": func(t *testing.T, dir string, _ *repodata.Repomd) {
			os.WriteFile(filepath.Join(dir, repomdPath), []byte("<repomd"), 0o644)
		},
		"no repomd.xml": func(t *testing.T, dir string, _ *repodata.Repomd) {
			os.Remove(filepath.Join(dir, repomdPath))
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, md := publishedRepo(t)
			damage(t, dir, md)
			rep := checkMetadataDir(t, dir)
			if rep.Usable() || len(rep.Problems) == 0 {
				t.Errorf("usable = %v, problems:\n%s", rep.Usable(), problemText(rep.Problems))
			}
		})
	}
}

// A document larger than repomd.xml records is refused after one byte past
// its size, not buffered whole.
func TestCheckMetadataStopsReadingAnOversizedDocument(t *testing.T) {
	dir, md := publishedRepo(t)
	loc := filepath.Join(dir, md.DataEntryByType("other").Location)
	f, err := os.OpenFile(loc, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(bytes.Repeat([]byte{0}, 1<<20))
	f.Close()
	if got := problemText(checkMetadataDir(t, dir).Problems); !strings.Contains(got, "larger than the") {
		t.Errorf("problems = %q", got)
	}
}

// replaceDoc republishes the typ document with edit applied to its XML.
func replaceDoc(t *testing.T, dir string, md *repodata.Repomd, typ string, edit func(string) string) {
	t.Helper()
	for i, d := range md.Data {
		if d.Type != typ {
			continue
		}
		f, err := os.Open(filepath.Join(dir, d.Location))
		if err != nil {
			t.Fatal(err)
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		var open bytes.Buffer
		open.ReadFrom(zr)
		f.Close()
		md.Data = append(md.Data[:i], md.Data[i+1:]...)
		addDocument(t, dir, md, typ, []byte(edit(open.String())))
		return
	}
	t.Fatalf("no %s document", typ)
}

// Defects only visible by reading the documents against each other: each
// is a sound file, so nothing before this caught them.
func TestCheckMetadataCrossChecksTheDocuments(t *testing.T) {
	for _, tc := range []struct {
		name   string
		typ    string
		edit   func(string) string
		want   string
		isWarn bool
	}{
		{"package missing from filelists", typeFilelists, func(s string) string {
			i, j := strings.Index(s, "<package "), strings.Index(s, "</package>")
			return s[:i] + s[j+len("</package>"):]
		}, "missing from filelists.xml", false},
		{"package missing from other", typeOther, func(s string) string {
			i, j := strings.Index(s, "<package "), strings.Index(s, "</package>")
			return s[:i] + s[j+len("</package>"):]
		}, "missing from other.xml", true},
		{"filelists names another package", typeFilelists, func(s string) string {
			return strings.Replace(s, `name="`, `name="x`, 1)
		}, "gives its checksum to", false},
		{"duplicate package", typePrimary, func(s string) string {
			i, j := strings.Index(s, "<package "), strings.Index(s, "</package>")+len("</package>")
			return s[:j] + s[i:j] + s[j:]
		}, "lists its checksum", false},
		{"xml:base", typePrimary, func(s string) string {
			return strings.Replace(s, "<location ", `<location xml:base="https://elsewhere.example/" `, 1)
		}, "xml:base", true},
		{"wrong declared count", typePrimary, func(s string) string {
			return strings.Replace(s, `packages="`, `packages="9`, 1)
		}, "says it lists", true},
		{"weak package checksum", typePrimary, func(s string) string {
			return strings.Replace(s, `<checksum type="sha256"`, `<checksum type="sha1"`, 1)
		}, "weak checksum", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, md := publishedRepo(t)
			replaceDoc(t, dir, md, tc.typ, tc.edit)
			rep := checkMetadataDir(t, dir)
			got, other := problemText(rep.Problems), problemText(rep.Warnings)
			if tc.isWarn {
				got, other = other, got
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("want %q in %s; problems:\n%s\nwarnings:\n%s", tc.want, map[bool]string{true: "warnings", false: "problems"}[tc.isWarn], problemText(rep.Problems), problemText(rep.Warnings))
			}
			if strings.Contains(other, tc.want) {
				t.Errorf("%q reported as the wrong severity", tc.want)
			}
		})
	}
}

// A sound RPM whose header is not what primary.xml describes: dnf resolves
// with the metadata and installs the header, so they must agree.
func TestVerifyComparesEachHeaderWithTheMetadata(t *testing.T) {
	dir, md := publishedRepo(t)
	replaceDoc(t, dir, md, typePrimary, func(s string) string {
		s = strings.Replace(s, `rel="3"`, `rel="4"`, 1)
		return strings.Replace(s, `<rpm:header-range start="`, `<rpm:header-range start="1`, 1)
	})
	got := problemText(verifyDir(t, dir, VerifyOptions{}).Problems)
	for _, want := range []string{"its header says it is hello-0:2.10-3.noarch, but the metadata says hello-0:2.10-4.noarch", "header-range says"} {
		if !strings.Contains(got, want) {
			t.Errorf("problems lack %q:\n%s", want, got)
		}
	}
}

func TestCheckMetadataFlagsADuplicateType(t *testing.T) {
	dir, md := publishedRepo(t)
	md.Data = append(md.Data, *md.DataEntryByType("other"))
	rewriteRepomd(t, dir, md)
	if got := problemText(checkMetadataDir(t, dir).Problems); !strings.Contains(got, "more than once") {
		t.Errorf("problems = %q", got)
	}
}

func TestSelected(t *testing.T) {
	pkgs := []*repodata.Package{
		mkPkg("foo", "x86_64", "1.0", "1"),
		mkPkg("foo", "x86_64", "1.1", "1"),
		mkPkg("foo", "aarch64", "1.0", "1"),
		mkPkg("bar", "x86_64", "2.0", "1"),
		mkPkg("baz", "noarch", "3.0", "1"),
	}
	for _, p := range pkgs {
		p.PkgID, p.Location = p.NEVRA(), p.NEVRA()+".rpm"
	}
	r := &Repo{idx: repodata.Merge(pkgs, nil, nil)}
	nevras := func(opt VerifyOptions) string {
		var s []string
		for _, p := range r.Selected(opt) {
			s = append(s, p.NEVRA())
		}
		return strings.Join(s, " ")
	}
	for _, tc := range []struct {
		opt  VerifyOptions
		want []string
		not  []string
	}{
		{VerifyOptions{Arches: []string{"x86_64"}}, []string{"foo-1.0-1.x86_64", "bar-2.0-1.x86_64", "baz-3.0-1.noarch"}, []string{"aarch64"}},
		{VerifyOptions{Names: []string{"foo"}}, []string{"foo-1.0-1.aarch64"}, []string{"bar", "baz"}},
		{VerifyOptions{Names: []string{"foo"}, LatestOnly: true}, []string{"foo-1.1-1.x86_64", "foo-1.0-1.aarch64"}, []string{"foo-1.0-1.x86_64"}},
	} {
		got := nevras(tc.opt)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("Selected(%+v) = %q, missing %s", tc.opt, got, w)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(got, n) {
				t.Errorf("Selected(%+v) = %q, should not include %s", tc.opt, got, n)
			}
		}
	}
}
