package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestHistoricalDiscardIntentRetainsVerifiedWorkerAndUserQuote(t *testing.T) {
	original := `{"discard":true,"user_quote":"User said Rider must land","rider":{"generation":"original","launch":2,"agent_name":"posse-shop-t1-2","panes":[{"pane":{"pane_id":"owned:p1"},"shell":{"pid":123,"identity":"boot/start"}}]}}`
	var payload discardIntentPayload
	if err := json.Unmarshal([]byte(original), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Worker == nil || payload.Worker.Generation != "original" || payload.Worker.Launch != 2 || payload.Worker.Panes[0].Shell.Identity != "boot/start" || payload.UserQuote != "User said Rider must land" {
		t.Fatalf("upgrade lost discard authority: %#v", payload)
	}
	encoded, err := json.Marshal(payload)
	if err != nil || strings.Contains(string(encoded), `"rider"`) || !strings.Contains(string(encoded), `"worker"`) {
		t.Fatalf("new payload vocabulary=%s: %v", encoded, err)
	}
	conflict := strings.TrimSuffix(original, "}") + `,"worker":{"generation":"replacement"}}`
	if err := json.Unmarshal([]byte(conflict), &payload); err == nil {
		t.Fatal("conflicting Worker bindings were accepted")
	}
}

func TestUpgradeReplacesOnlyRecordedLegacyWatchPane(t *testing.T) {
	db, project, home := newWatchTestProject(t)
	defer db.Close()
	ctx := context.Background()
	if err := db.SetWatchRecovery(ctx, project.ID, store.WatchRecovery{PaneID: "legacy:p1"}); err != nil {
		t.Fatal(err)
	}
	legacy := herdr.Pane{PaneID: "legacy:p1", TabID: "legacy:t1", WorkspaceID: project.HerdrWorkspaceID, Label: "posse:" + project.Name + ":lookout"}
	foreign := herdr.Pane{PaneID: "foreign:p1", TabID: "foreign:t1", WorkspaceID: "unrelated", Label: legacy.Label}
	fake := herdr.NewFake()
	snapshot := herdr.Snapshot{Panes: []herdr.Pane{legacy, foreign}}
	fake.SnapshotValue = snapshot
	service := testService(home, fake)
	if err := service.ensureWatchTab(ctx, db, project, snapshot, true); err != nil {
		t.Fatal(err)
	}
	closed, created := false, false
	for _, call := range fake.Calls {
		if call.Method == "tab.close" {
			if call.Params["tab_id"] != legacy.TabID {
				t.Fatalf("upgrade closed an unrelated pane: %#v", call)
			}
			closed = true
		}
		if call.Method == "tab.create" && call.Params["label"] == watchTabLabel(project) {
			created = true
		}
	}
	if !closed || !created {
		t.Fatalf("upgrade did not replace the recorded observer: %#v", fake.Calls)
	}
}

func TestUpgradePreservesAgentOccupyingFormerWatchPane(t *testing.T) {
	db, project, home := newWatchTestProject(t)
	defer db.Close()
	ctx := context.Background()
	if err := db.SetWatchRecovery(ctx, project.ID, store.WatchRecovery{PaneID: "legacy:p1"}); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	snapshot := herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "legacy:p1", TabID: "legacy:t1", WorkspaceID: project.HerdrWorkspaceID, Label: "posse:" + project.Name + ":lookout", Agent: "pi"}}}
	if err := testService(home, fake).ensureWatchTab(ctx, db, project, snapshot, true); err == nil {
		t.Fatal("upgrade accepted an agent replacing the old observer")
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("upgrade touched the replacement agent: %#v", fake.Calls)
	}
}
