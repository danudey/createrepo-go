package repo

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/danudey/createrepo-go/pkg/rpm/repodata"
)

// A repository built by createrepo_c often publishes documents this tool does
// not generate. Adding a package must keep them: before, the first commit
// dropped comps, updateinfo and modules from repomd.xml and deleted the files.
// The sqlite form of primary describes the old package list, so it is dropped.
func TestCommitKeepsMetadataItDoesNotGenerate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	r, _ := newLocalRepo(t, dir, true)
	if _, err := r.AddRPM(helloRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r.Close()

	// Add the documents another tool would have published.
	repomdFile := filepath.Join(dir, repomdPath)
	data, err := os.ReadFile(repomdFile)
	if err != nil {
		t.Fatal(err)
	}
	md, err := repodata.ParseRepomd(data)
	if err != nil {
		t.Fatal(err)
	}
	extras := map[string]string{"group": "comps.xml", "updateinfo": "updateinfo.xml", "modules": "modules.yaml", "primary_db": "primary.sqlite"}
	for typ, name := range extras {
		body := []byte("<" + typ + "/>\n")
		href := "repodata/" + name
		if err := os.WriteFile(filepath.Join(dir, href), body, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256Hex(body)
		md.Data = append(md.Data, repodata.DataEntry{
			Type: typ, ChecksumType: "sha256", Checksum: sum, OpenChecksum: sum,
			Location: href, Timestamp: 1, Size: int64(len(body)), OpenSize: int64(len(body)),
		})
	}
	if err := os.WriteFile(repomdFile, repodata.WriteRepomd(md), 0o644); err != nil {
		t.Fatal(err)
	}

	r2, _ := newLocalRepo(t, dir, false)
	if _, err := r2.AddRPM(libfooRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r2.Close()

	data, _ = os.ReadFile(repomdFile)
	after, err := repodata.ParseRepomd(data)
	if err != nil {
		t.Fatal(err)
	}
	for typ, name := range extras {
		d := after.DataEntryByType(typ)
		_, statErr := os.Stat(filepath.Join(dir, "repodata", name))
		if typ == "primary_db" {
			if d != nil || statErr == nil {
				t.Errorf("the stale sqlite database was kept (entry %v, file err %v)", d, statErr)
			}
			continue
		}
		if d == nil || d.Location != "repodata/"+name {
			t.Errorf("%s is missing from the new repomd.xml (entry %+v)", typ, d)
		}
		if statErr != nil {
			t.Errorf("%s was deleted: %v", name, statErr)
		}
	}

	// The kept documents still load and verify against the new repomd.xml.
	r3, _ := newLocalRepo(t, dir, false)
	defer r3.Close()
	if doc, err := r3.MetadataDocument(ctx, "group"); err != nil || string(doc) != "<group/>\n" {
		t.Errorf("comps after the commit = (%q, %v)", doc, err)
	}
}

// copy --overwrite replaces the destination wholesale (ClearIndex), so the old
// destination's other documents describe a repository that no longer exists
// and must go with it.
func TestClearIndexDropsMetadataItDoesNotGenerate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	r, _ := newLocalRepo(t, dir, true)
	if _, err := r.AddRPM(helloRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r.Close()

	repomdFile := filepath.Join(dir, repomdPath)
	data, _ := os.ReadFile(repomdFile)
	md, err := repodata.ParseRepomd(data)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("<comps/>\n")
	os.WriteFile(filepath.Join(dir, "repodata/comps.xml"), body, 0o644)
	sum := sha256Hex(body)
	md.Data = append(md.Data, repodata.DataEntry{
		Type: "group", ChecksumType: "sha256", Checksum: sum,
		OpenChecksum: sum, Location: "repodata/comps.xml", Size: int64(len(body)), OpenSize: int64(len(body)),
	})
	os.WriteFile(repomdFile, repodata.WriteRepomd(md), 0o644)

	r2, _ := newLocalRepo(t, dir, false)
	r2.ClearIndex()
	if _, err := r2.AddRPM(libfooRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r2.Close()

	data, _ = os.ReadFile(repomdFile)
	after, _ := repodata.ParseRepomd(data)
	if after.DataEntryByType("group") != nil {
		t.Error("a replaced repository kept the old comps entry")
	}
	if _, err := os.Stat(filepath.Join(dir, "repodata/comps.xml")); err == nil {
		t.Error("a replaced repository kept the old comps file")
	}
}
