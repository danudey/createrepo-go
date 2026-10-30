package repocheck

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danudey/createrepo-go/pkg/rpm/repo"
)

// A .repo file fetched over HTTP was written by whoever serves it, so the
// baseurls it names must be http(s): a file:// baseurl would have check read
// the operator's local files. The same file on local disk is the operator's
// own and is checked.
func TestRemoteRepoFileCannotNameLocalPaths(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	r, err := repo.Open(ctx, dir, repo.Options{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddRPM("../../../reference/rpm/rpmbuild/RPMS/noarch/hello-2.10-3.noarch.rpm"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r.Close()

	repoFile := "[local]\nname=local\nbaseurl=file://" + dir + "\nenabled=1\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, repoFile)
	}))
	defer srv.Close()

	results, _, err := Run(ctx, Options{Input: srv.URL + "/evil.repo"})
	if err != nil {
		t.Fatal(err)
	}
	ok, fail, _, _ := Summarize(results)
	if ok != 0 || fail != 1 || !strings.Contains(results[0].Detail, "may only name http(s)") {
		t.Errorf("a fetched .repo naming file:// was checked (ok %d, fail %d): %+v", ok, fail, results)
	}

	local := filepath.Join(t.TempDir(), "local.repo")
	if err := os.WriteFile(local, []byte(repoFile), 0o644); err != nil {
		t.Fatal(err)
	}
	results, _, err = Run(ctx, Options{Input: local})
	if err != nil {
		t.Fatal(err)
	}
	if ok, fail, _, _ := Summarize(results); ok == 0 || fail != 0 {
		t.Errorf("a local .repo naming file:// was not checked (ok %d, fail %d): %+v", ok, fail, results)
	}
}
