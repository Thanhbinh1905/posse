package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestRecoveryDoesNotUndoFailedSignalCommittedAfterTaskEnumeration(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo, home := filepath.Join(root, "repo"), filepath.Join(root, "posse")
	initRepo(t, repo)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	config := "[lead]\nkind = \"claude\"\n\n[profiles.deep]\nkind = \"claude\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{
		Seq: 1, Type: "ship", Title: "Failed Signal recovery", Profile: "deep", LandingMode: "local",
		Branch: "posse/t1", BaseRef: "main", HerdrWorkspaceID: "w2", PaneID: "w2:p1", PaneLabel: "posse:shop:t1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "Rider ready"); err != nil {
		t.Fatal(err)
	}
	mount, err := db.AcquireMount(ctx, project.ID, taskID, filepath.Join(home, "remuda"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(mount.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "worktree", "add", "-b", "posse/t1", mount.Path, "main")
	if _, err := db.NextTaskLaunch(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTaskLaunch(ctx, taskID, mount.Path, "w2", "w2:p1", "posse:shop:t1", "old-rider"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTaskObservation(ctx, taskID, "w2:p1", "w2", "", 0, 0, "same-server"); err != nil {
		t.Fatal(err)
	}
	if err := db.RememberProjectServerStartedAt(ctx, project.ID, "same-server"); err != nil {
		t.Fatal(err)
	}
	before, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}

	adapter := &failedSignalRecoveryAdapter{
		Fake:     herdr.NewFake(),
		snapshot: herdr.Snapshot{ServerStartedAt: "same-server"},
	}
	adapter.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
	adapter.onSecondSnapshot = func() {
		if state, err := db.RecordWorkerSignal(ctx, before, "failed", "stop this Rider", nil, "task_failed"); err != nil || state != store.StateFailed {
			t.Fatalf("record failed Signal: state=%s err=%v", state, err)
		}
	}
	if _, err := testService(home, adapter).recoverProject(ctx, db, home, project); err != nil {
		t.Fatalf("recovery: %v", err)
	}

	after, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != store.StateFailed || after.Launches != before.Launches || after.MountID != before.MountID || after.WorktreePath != before.WorktreePath || after.Branch != before.Branch || adapter.CallCount("agent.start") != 1 {
		t.Fatalf("recovery undid failed Signal: state=%s launches=%d want=%d mount=%d want=%d worktree=%q branch=%q agent_starts=%d", after.State, after.Launches, before.Launches, after.MountID, before.MountID, after.WorktreePath, after.Branch, adapter.CallCount("agent.start"))
	}
}

type failedSignalRecoveryAdapter struct {
	*herdr.Fake
	snapshot         herdr.Snapshot
	snapshots        int
	onSecondSnapshot func()
}

func (a *failedSignalRecoveryAdapter) Snapshot(ctx context.Context) (herdr.Snapshot, error) {
	a.snapshots++
	if a.snapshots == 2 && a.onSecondSnapshot != nil {
		a.onSecondSnapshot()
	}
	return a.snapshot, nil
}

func (a *failedSignalRecoveryAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	switch method {
	case "workspace.create":
		a.Results[method] = json.RawMessage(`{"workspace":{"workspace_id":"w3"},"root_pane":{"pane_id":"w3:p1","tab_id":"w3:t1"}}`)
		a.snapshot.Workspaces = append(a.snapshot.Workspaces, herdr.Workspace{WorkspaceID: "w3", Label: "Lead:shop"})
	case "worktree.open":
		a.Results[method] = json.RawMessage(`{"workspace":{"workspace_id":"w4"},"root_pane":{"pane_id":"w4:p1","tab_id":"w4:t1"}}`)
		a.snapshot.Panes = append(a.snapshot.Panes, herdr.Pane{PaneID: "w4:p1", WorkspaceID: "w4", TabID: "w4:t1", Label: "posse:shop:t1"})
	case "tab.create":
		if params["label"] == "posse:shop:lead" {
			a.Results[method] = json.RawMessage(`{"tab":{"tab_id":"w3:t1"},"root_pane":{"pane_id":"w3:p1","tab_id":"w3:t1"}}`)
			a.snapshot.Panes = append(a.snapshot.Panes, herdr.Pane{PaneID: "w3:p1", WorkspaceID: "w3", TabID: "w3:t1", Label: "posse:shop:lead"})
		} else {
			delete(a.Results, method)
		}
	case "agent.start":
		for i := range a.snapshot.Panes {
			if a.snapshot.Panes[i].PaneID == params["pane_id"] {
				a.snapshot.Panes[i].Agent = "claude"
				a.snapshot.Panes[i].AgentStatus = "working"
			}
		}
	}
	return a.Fake.Call(ctx, method, params)
}
