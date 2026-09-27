package app

import (
	"context"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLookoutProcessListingMatchesPaneAndPosseHome(t *testing.T) {
	listing := "12 /usr/local/bin/posse lookout --poll-only HERDR_PANE_ID=w1:p9 POSSE_HOME=/tmp/other HOME=/tmp\n" +
		"13 /usr/local/bin/posse lookout --poll-only HERDR_PANE_ID=w1:p2 POSSE_HOME=/tmp/shop HOME=/tmp\n"
	if !lookoutProcessInListing(listing, "w1:p2", "/tmp/shop") {
		t.Fatal("Darwin process listing missed matching Lookout")
	}
	if lookoutProcessInListing(listing, "w1:p9", "/tmp/shop") {
		t.Fatal("mixed separate Lookout homes")
	}
}

func TestUnknownLookoutProcessIsReplacedWithoutTypingIntoOldPane(t *testing.T) {
	home := t.TempDir()
	fake := herdr.NewFake()
	project := store.Project{Name: "shop", Root: home, HerdrWorkspaceID: "w1"}
	pane := herdr.Pane{PaneID: "stale:p9", TabID: "stale:t9", WorkspaceID: "w1", Label: lookoutTabLabel(project)}
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{pane}}
	service := testService(home, fake)
	t.Setenv("HERDR_PANE_ID", "")
	if err := service.ensureLookoutTab(context.Background(), project, fake.SnapshotValue); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("tab.close") != 1 || fake.CallCount("tab.create") != 1 {
		t.Fatalf("stale tab was not replaced: %#v", fake.Calls)
	}
	for _, call := range fake.Calls {
		if call.Method == "pane.send_input" && call.Params["pane_id"] == pane.PaneID {
			t.Fatal("queued a new command into an unknown live Lookout pane")
		}
	}
}
