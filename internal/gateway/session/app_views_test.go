package session

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestSaveToolUIView_CarriesSessionID(t *testing.T) {
	p := &fakePersistence{}
	ui := &types.ToolUIRef{ViewID: "v1", ServerID: "s", ResourceURI: "ui://s/x", ToolName: "t"}
	if err := SaveToolUIView(context.Background(), p, "sess-1", ui); err != nil {
		t.Fatal(err)
	}
	if len(p.appViews) != 1 || p.appViews[0].SessionID != "sess-1" || p.appViews[0].ViewID != "v1" {
		t.Fatalf("appViews = %+v", p.appViews)
	}
}

func TestLinkToolUIViews_SkipsWhenNothingToLink(t *testing.T) {
	p := &fakePersistence{}
	ctx := context.Background()
	if err := LinkToolUIViews(ctx, p, "s", nil, 5); err != nil {
		t.Fatal(err)
	}
	if err := LinkToolUIViews(ctx, p, "s", []string{"v1"}, 0); err != nil {
		t.Fatal(err)
	}
	if len(p.linkedTo) != 0 {
		t.Fatalf("must not link, got %v", p.linkedTo)
	}
	if err := LinkToolUIViews(ctx, p, "s", []string{"v1"}, 7); err != nil || p.linkedTo["v1"] != 7 {
		t.Fatalf("err=%v linked=%v", err, p.linkedTo)
	}
}
