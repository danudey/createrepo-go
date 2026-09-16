package repoconfig

import (
	"context"
	"testing"

	"github.com/danudey/createrepo-go/pkg/backend"
)

func newBackend(t *testing.T) backend.Backend {
	t.Helper()
	be, err := backend.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	return be
}

func TestLoadMissingReturnsNil(t *testing.T) {
	be := newBackend(t)
	cfg, err := Load(context.Background(), be)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected nil config for a repo with no config file, got %+v", cfg)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	be := newBackend(t)
	want := &Config{
		Name:            "Frobulator Beta",
		BaseURL:         "https://downloads.example.com/ee/rpms/el9",
		LocationPrefix:  "Packages",
		Target:          "rhel9",
		SignPackages:    true,
		SignMetadata:    true,
		SignatureFormat: "v4",
		GPGKeyID:        "ABCDEF0123456789ABCDEF0123456789ABCDEF01",
	}
	if err := Save(context.Background(), be, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(context.Background(), be)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil {
		t.Fatal("expected a config after Save, got nil")
	}
	if *got != *want {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", *got, *want)
	}
}

func TestSaveOverwrites(t *testing.T) {
	be := newBackend(t)
	if err := Save(context.Background(), be, &Config{Name: "old", SignPackages: true}); err != nil {
		t.Fatal(err)
	}
	if err := Save(context.Background(), be, &Config{Name: "new"}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(context.Background(), be)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "new" || got.SignPackages {
		t.Errorf("overwrite failed: got %+v", *got)
	}
}
