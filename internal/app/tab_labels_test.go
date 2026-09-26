package app

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestRelabelProjectTabsUsesHerdrOrderAndLeavesUserTabsAlone(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	project.HerdrWorkspaceID = "w1"
	project.LeadPaneID = "p1"
	project.LeadLabel = "posse:shop:lead"
	for _, task := range []store.Task{
		{Seq: 1, Type: "ship", Title: "First rider", ShortName: "first-rider", State: store.StateSpawning, LandingMode: "local", HerdrWorkspaceID: "w1", PaneID: "p4", PaneLabel: "posse:shop:t1"},
		{Seq: 2, Type: "ship", Title: "Second rider", ShortName: "second-rider", State: store.StateSpawning, LandingMode: "local", HerdrWorkspaceID: "w1", PaneID: "p3", PaneLabel: "posse:shop:t2"},
	} {
		if _, err := db.CreateTask(ctx, project.ID, task); err != nil {
			t.Fatal(err)
		}
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{
		Tabs: []herdr.Tab{
			{TabID: "t1", WorkspaceID: "w1", Label: "1"},
			{TabID: "t2", WorkspaceID: "w1", Label: "user-shell"},
			{TabID: "t3", WorkspaceID: "w1", Label: "second-rider"},
			{TabID: "t4", WorkspaceID: "w1", Label: "first-rider"},
		},
		Panes: []herdr.Pane{
			{PaneID: "p1", WorkspaceID: "w1", TabID: "t1", Label: project.LeadLabel},
			{PaneID: "p2", WorkspaceID: "w1", TabID: "t2"},
			{PaneID: "p3", WorkspaceID: "w1", TabID: "t3", Label: "posse:shop:t2"},
			{PaneID: "p4", WorkspaceID: "w1", TabID: "t4", Label: "posse:shop:t1"},
		},
	}

	testService(t.TempDir(), fake).relabelProjectTabs(ctx, db, project)

	got := map[string]string{}
	for _, call := range fake.Calls {
		if call.Method == "tab.rename" {
			got[call.Params["tab_id"].(string)] = call.Params["label"].(string)
		}
	}
	want := map[string]string{"t1": "Lead", "t3": "├─ second-rider", "t4": "└─ first-rider"}
	if len(got) != len(want) {
		t.Fatalf("tab renames = %#v, want %#v", got, want)
	}
	for tabID, label := range want {
		if got[tabID] != label {
			t.Errorf("tab %s relabeled as %q, want %q", tabID, got[tabID], label)
		}
	}
	if _, renamed := got["t2"]; renamed {
		t.Fatalf("User tab was renamed: %#v", got)
	}
}

func TestRelabelProjectTabsLogsFailuresAndContinues(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	project.LeadPaneID = "p1"
	project.LeadLabel = "posse:shop:lead"
	if _, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "First rider", ShortName: "first-rider", State: store.StateSpawning, LandingMode: "local", WorktreePath: t.TempDir(), HerdrWorkspaceID: "w1", PaneID: "p2", PaneLabel: "posse:shop:t1"}); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{
		Tabs:  []herdr.Tab{{TabID: "t1", Label: "1"}, {TabID: "t2", Label: "first-rider"}},
		Panes: []herdr.Pane{{PaneID: "p1", TabID: "t1", Label: project.LeadLabel}, {PaneID: "p2", TabID: "t2", Label: "posse:shop:t1"}},
	}
	fake.Errors["tab.rename"] = &herdr.Error{Code: "tab_unavailable", Message: "rename denied"}
	service := testService(t.TempDir(), fake)

	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	service.relabelProjectTabs(ctx, db, project)

	if fake.CallCount("tab.rename") != 2 {
		t.Fatalf("rename failure stopped attempts: %#v", fake.Calls)
	}
	if !strings.Contains(output.String(), "could not label Herdr tab t1") || !strings.Contains(output.String(), "could not label Herdr tab t2") {
		t.Fatalf("rename errors were not logged: %s", output.String())
	}
}
