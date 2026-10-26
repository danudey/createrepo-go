package repo

import (
	"context"
	"errors"
	"testing"

	"github.com/danudey/createrepo-go/pkg/publish"
)

// Two runs that open the same repository and both commit: the second must not
// replace the first's repomd.xml, which would silently drop the package the
// first one added.
func TestCommitDetectsAConcurrentPublish(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	seed, _ := newLocalRepo(t, dir, true)
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	seed.Close()

	first, _ := newLocalRepo(t, dir, false)
	second, _ := newLocalRepo(t, dir, false)
	defer first.Close()
	defer second.Close()
	if _, err := first.AddRPM(helloRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := second.AddRPM(libfooRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Commit(ctx); !errors.Is(err, publish.ErrConcurrentPublish) {
		t.Fatalf("the second commit = %v, want ErrConcurrentPublish", err)
	}

	after, _ := newLocalRepo(t, dir, false)
	defer after.Close()
	if len(after.Index().FindByNEVRA("hello", "noarch", "")) != 1 {
		t.Error("the first commit's package was lost")
	}

	// The run that published can keep committing on top of its own work.
	if _, err := first.AddRPM(libfooRPM()); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Commit(ctx); err != nil {
		t.Errorf("a second commit from the same run: %v", err)
	}
}
