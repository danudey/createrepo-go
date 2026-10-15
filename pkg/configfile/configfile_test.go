package configfile

import (
	"context"
	"testing"

	"github.com/danudey/createrepo-go/pkg/backend"
)

type settings struct {
	Name string `json:"name"`
	On   bool   `json:"on"`
}

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	be, err := backend.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Load[settings](ctx, be, "tool.json"); got != nil || err != nil {
		t.Fatalf("Load of an absent file = %v, %v; want nil, nil", got, err)
	}
	if err := Save(ctx, be, "tool.json", &settings{Name: "x", On: true}); err != nil {
		t.Fatal(err)
	}
	got, err := Load[settings](ctx, be, "tool.json")
	if err != nil || got == nil || *got != (settings{Name: "x", On: true}) {
		t.Errorf("Load = %+v, %v; want the saved settings", got, err)
	}
}
