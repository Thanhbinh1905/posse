package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestSweepOrphanPanesDryRunThenClose(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	posseHome := filepath.Join(root, "posse")
	mount := filepath.Join(root, "orphan-mount")
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(posseHome)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state) VALUES(?,1,?,'idle')`, project.ID, mount); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fake := &changingSnapshotAdapter{Fake: herdr.NewFake(), snapshot: herdr.Snapshot{Panes: []herdr.Pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "working"},
		{PaneID: "w2:p1", WorkspaceID: "w2", Label: "posse:shop:t99", CWD: mount},
	}}}
	service := testService(posseHome, fake)
	t.Chdir(repo)
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out = &output
	if code := cli.Run([]string{"sweep"}); code != 0 || !strings.Contains(output.String(), "dry_run: true") || !strings.Contains(output.String(), "w2:p1") {
		t.Fatalf("sweep dry run did not list the orphan: code=%d output=%s", code, output.String())
	}
	if fake.CallCount("pane.close") != 0 {
		t.Fatalf("dry run closed an orphan pane: %#v", fake.Calls)
	}
	db, err = store.Open(posseHome)
	if err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByName(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 || notices[0].Kind != "orphan_pane" {
		t.Fatalf("orphan detection did not create exactly one Notice: %#v", notices)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if code := cli.Run([]string{"sweep", "--yes"}); code != 0 || !strings.Contains(output.String(), "w2:p1") {
		t.Fatalf("sweep --yes failed to close the orphan: code=%d output=%s", code, output.String())
	}
	if fake.CallCount("pane.close") != 1 {
		t.Fatalf("sweep --yes closed unexpected panes: %#v", fake.Calls)
	}
	for _, pane := range fake.currentSnapshot().Panes {
		if pane.PaneID == "w2:p1" || pane.Label == "posse:shop:t99" {
			t.Fatalf("orphan remains after sweep: %#v", pane)
		}
	}
}

func TestTaskPaneCwdOwnershipResolvesSymlinks(t *testing.T) {
	root := t.TempDir()
	mount := filepath.Join(root, "mount")
	if err := os.MkdirAll(filepath.Join(mount, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "mount-alias")
	if err := os.Symlink(mount, alias); err != nil {
		t.Fatal(err)
	}
	project := store.Project{Name: "shop"}
	task := store.Task{Seq: 4, WorktreePath: mount, HerdrWorkspaceID: "w2", PaneLabel: "posse:shop:t4"}
	pane := herdr.Pane{PaneID: "w2:p1", WorkspaceID: "w2", TabID: "w2:t1", CWD: filepath.Join(alias, "nested")}
	if !ownsTaskPane(herdr.Snapshot{}, project, task, map[string]bool{"w2:t1": true}, pane) {
		t.Fatal("symlinked cwd inside the Task Mount was not recognized")
	}
	if ownsTaskPane(herdr.Snapshot{}, project, task, map[string]bool{"w2:t2": true}, pane) {
		t.Fatal("an unlabeled pane outside the Task's tab was claimed by its cwd")
	}
}

func TestTornDownForeignPaneCreatesNoticeWithoutBlockingProject(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	label := "posse:shop:t1"
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "scout", Title: "Torn down", LandingMode: "local", HerdrWorkspaceID: "w2", PaneID: "w2:p1", PaneLabel: label})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET state='torn-down' WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{
		Panes:  []herdr.Pane{{PaneID: "w3:p9", WorkspaceID: "w3", Label: label, Agent: "claude", CWD: repo}},
		Agents: []herdr.Agent{{Name: "other-agent", PaneID: "w3:p9"}},
	}
	service := testService(home, fake)
	if _, err := service.prepareProject(ctx, db, project); err != nil {
		t.Fatalf("foreign pane Notice blocked Project reconciliation: %v", err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "unsaddle_incomplete" {
		t.Fatalf("teardown did not leave one unsaddle_incomplete Notice: %#v, %v", notices, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}
